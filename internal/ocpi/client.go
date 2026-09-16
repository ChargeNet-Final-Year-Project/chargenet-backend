package ocpi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// The outbound half of OCPI 2.3.0: until OCPI receivers, ChargeNet only
// *received* a CPO's locations, tariffs, sessions, CDRs and command results.
// [Client] is the other direction — the HTTP calls ChargeNet makes to a partner,
// authenticated with the Token B the credentials exchange handed it.

// DefaultTimeout keeps an unreachable CPO from holding a driver's command open.
const DefaultTimeout = 5 * time.Second

// maxResponseBody caps what ChargeNet reads back from a partner. OCPI documents
// are small, and a partner is not trusted to answer with a bounded one.
const maxResponseBody = 1 << 20

// The tracing headers ChargeNet sends on every call. Neither is defined by
// OCPI; both are common tracing headers and both are named as they are
// spelled in the wild, so a partner that logs them can correlate its side.
const (
	headerRequestID     = "X-Request-ID"
	headerCorrelationID = "X-Correlation-ID"
)

// Client performs outbound OCPI calls against a CPO partner. It is deliberately
// thin: it knows how to authenticate, bound, retry and unwrap one request, and
// nothing about what any particular module means — that is [CommandClient]'s
// job, and one level of "which body goes where" is enough.
//
// A *Client is safe for concurrent use.
type Client struct {
	http    *http.Client
	timeout time.Duration
}

// Option configures a [Client] at construction.
type Option func(*Client)

// WithHTTPClient replaces the transport. Tests use an httptest server's own
// client; production keeps the shared one.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithTimeout overrides [DefaultTimeout] for one logical call (each of the two
// attempts is bounded by the same value).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// NewClient builds a client with [DefaultTimeout] unless overridden.
func NewClient(opts ...Option) *Client {
	c := &Client{http: &http.Client{Timeout: DefaultTimeout}, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Do sends one OCPI request to a partner and returns the payload of the success
// envelope it answers with.
//
// The call carries `Authorization: Token {token_b}` and both tracing
// headers; a 5xx is retried once, because a partner restarting mid-command is
// worth a second try and a 4xx is not. Anything that stops the call from
// producing a success payload — no token, no base URL, a dead host, a timeout, a
// non-2xx status, an OCPI failure code — is reported as CPO_UNREACHABLE, which is
// the one thing the session manager's orchestrator can act on.
func (c *Client) Do(ctx context.Context, cpo *store.CpoAccount, method, path string, body any) (json.RawMessage, error) {
	if cpo == nil {
		return nil, gateway.Errorf(gateway.CodeCPOUnreachable, "no CPO to call")
	}
	if strings.TrimSpace(cpo.BaseURL) == "" {
		return nil, gateway.Errorf(gateway.CodeCPOUnreachable, "CPO %s has no base URL configured", cpo.ID)
	}
	if strings.TrimSpace(cpo.TokenB) == "" {
		return nil, gateway.Errorf(gateway.CodeCPOUnreachable, "ChargeNet holds no token for CPO %s", cpo.ID)
	}

	url := strings.TrimRight(cpo.BaseURL, "/") + path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == VersionNumber {
		if endpoint := cpo.ModuleEndpoints[parts[1]]; endpoint != "" {
			url = strings.TrimRight(endpoint, "/")
			if len(parts) > 2 {
				url += "/" + strings.Join(parts[2:], "/")
			}
		}
	}
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, gateway.Errorf(gateway.CodeValidation, "the %s body could not be encoded: %v", method, err)
		}
		payload = encoded
	}

	data, retry, err := c.attempt(ctx, cpo, method, url, payload)
	if err != nil && retry {
		// One retry, and only for a 5xx: the partner may be restarting, and the
		// command may not have been processed.
		data, _, err = c.attempt(ctx, cpo, method, url, payload)
	}
	return data, err
}

// attempt makes one request and reports whether a retry is worth trying. The
// `retry` flag is set only for a 5xx; every other failure is final.
func (c *Client) attempt(ctx context.Context, cpo *store.CpoAccount, method, url string, payload []byte) (json.RawMessage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, false, gateway.Errorf(gateway.CodeCPOUnreachable, "%s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(cpo.TokenB)))
	req.Header.Set("Accept", "application/json")
	correlation := newTraceID()
	req.Header.Set(headerRequestID, newTraceID())
	req.Header.Set(headerCorrelationID, correlation)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, gateway.Errorf(gateway.CodeCPOUnreachable, "%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, false, gateway.Errorf(gateway.CodeCPOUnreachable, "read %s %s: %v", method, url, err)
	}

	if resp.StatusCode >= 500 {
		return nil, true, gateway.Errorf(gateway.CodeCPOUnreachable,
			"%s %s: the CPO answered %d", method, url, resp.StatusCode)
	}
	data, err := unwrap(url, resp.StatusCode, raw)
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

// unwrap returns the payload of an OCPI envelope, or a CPO_UNREACHABLE error
// describing why there is none.
//
// A wrapped answer is honoured whatever its HTTP status: the envelope's
// `status_code` is the OCPI truth, and 1000 is the only success. A body with no
// envelope at all is accepted on a 2xx as the bare payload — the same
// tolerance the receivers show a CPO's pushes — and is a failure otherwise.
func unwrap(url string, httpStatus int, raw []byte) (json.RawMessage, error) {
	var env struct {
		Data          json.RawMessage `json:"data"`
		StatusCode    *int            `json:"status_code"`
		StatusMessage string          `json:"status_message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.StatusCode != nil {
		if *env.StatusCode != StatusSuccess {
			return nil, gateway.Errorf(gateway.CodeCPOUnreachable,
				"%s: OCPI %d: %s", url, *env.StatusCode, firstNonEmpty(env.StatusMessage, "no message"))
		}
		return env.Data, nil
	}
	if httpStatus/100 != 2 {
		return nil, gateway.Errorf(gateway.CodeCPOUnreachable, "%s: the CPO answered %d", url, httpStatus)
	}
	return json.RawMessage(raw), nil
}

// newTraceID uses the clock if random bytes are unavailable. The value only
// needs to be unique for request correlation.
func newTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ocpi-" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return "ocpi-" + hex.EncodeToString(b[:])
}

// firstNonEmpty returns the first of its arguments that is not blank.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

package emulator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// DefaultHeartbeat is how often the emulator re-pushes its locations and
// tariffs.
const DefaultHeartbeat = 60 * time.Second

// DefaultTick is the real-time cadence a running session is stepped at, the same
// as the in-process simulator.
const DefaultTick = time.Second

// DefaultScale is the simulated seconds per real second when none is configured;
// it matches SIM_TIME_SCALE's default.
const DefaultScale = 10

// requestTimeout bounds one outbound push to ChargeNet.
const requestTimeout = 5 * time.Second

// maxBody caps what the emulator reads back from ChargeNet.
const maxBody = 1 << 20

// Emulator is one emulated OCPI CPO for one [Profile]. It is safe for
// concurrent use: ChargeNet may send it several commands at once while the
// heartbeat and running sessions push back.
type Emulator struct {
	profile Profile
	store   *store.Store
	http    *http.Client
	// origin is ChargeNet's origin (PUBLIC_BASE_URL): where the emulator's
	// pushes and command callbacks go.
	origin string
	// scale is the simulated seconds per real second a session is stepped at.
	scale int
	tick  time.Duration
	beat  time.Duration

	// runCtx is cancelled by Close; every session goroutine and the heartbeat
	// observe it.
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	rng      *rand.Rand
	seq      int
	cdrSeq   int
	closed   bool
	statuses map[string]string // OCPI EVSE uid -> canonical status
	sessions map[string]*session
	byRef    map[string]*session
	chaos    chaos
}

// Option configures an [Emulator] at construction.
type Option func(*Emulator)

// WithHTTPClient replaces the transport used for outbound pushes. Tests point it
// at an httptest server's own client; production keeps the default.
func WithHTTPClient(c *http.Client) Option {
	return func(e *Emulator) {
		if c != nil {
			e.http = c
		}
	}
}

// WithOrigin overrides ChargeNet's origin. It defaults to the configured
// PUBLIC_BASE_URL; it exists so a test can point the emulator's pushes at the
// server it is actually serving on.
func WithOrigin(origin string) Option {
	return func(e *Emulator) {
		if s := strings.TrimRight(strings.TrimSpace(origin), "/"); s != "" {
			e.origin = s
		}
	}
}

// WithTick overrides the real-time session tick. Tests use a few milliseconds so
// a session finishes in the blink of an eye; production keeps [DefaultTick].
func WithTick(d time.Duration) Option {
	return func(e *Emulator) {
		if d > 0 {
			e.tick = d
		}
	}
}

// WithHeartbeat overrides the location/tariff re-push cadence. Tests set it to
// zero to disable the heartbeat entirely.
func WithHeartbeat(d time.Duration) Option {
	return func(e *Emulator) {
		if d >= 0 {
			e.beat = d
		}
	}
}

// WithTimeScale overrides the simulated seconds per real second.
func WithTimeScale(n int) Option {
	return func(e *Emulator) {
		if n > 0 {
			e.scale = n
		}
	}
}

// WithRand replaces the jitter source so a run is repeatable.
func WithRand(r *rand.Rand) Option {
	return func(e *Emulator) {
		if r != nil {
			e.rng = r
		}
	}
}

// New builds the emulator for one profile over a store and the platform config.
// The config supplies ChargeNet's origin (PUBLIC_BASE_URL) and the simulation
// time scale; both can be overridden by options.
func New(cfg config.Config, st *store.Store, prof Profile, opts ...Option) *Emulator {
	e := &Emulator{
		profile:  prof,
		store:    st,
		http:     &http.Client{Timeout: requestTimeout},
		origin:   strings.TrimRight(cfg.PublicBaseURL, "/"),
		scale:    cfg.SimTimeScale,
		tick:     DefaultTick,
		beat:     DefaultHeartbeat,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		statuses: make(map[string]string),
		sessions: make(map[string]*session),
		byRef:    make(map[string]*session),
	}
	if e.scale < 1 {
		e.scale = DefaultScale
	}
	for _, opt := range opts {
		opt(e)
	}
	e.runCtx, e.cancel = context.WithCancel(context.Background())
	return e
}

// Profile is the emulator's identity.
func (e *Emulator) Profile() Profile { return e.profile }

// SelfURL is the emulator's own OCPI root as ChargeNet reaches it, e.g.
// http://localhost:8080/emulator/voltgo/ocpi.
func (e *Emulator) SelfURL() string { return e.origin + e.profile.basePath() }

// Mount installs the emulator's OCPI endpoints on mux, under
// /emulator/{slug}/ocpi.
func (e *Emulator) Mount(mux *http.ServeMux) {
	base := e.profile.basePath()
	versionRoot := base + "/" + ocpi.VersionNumber

	mux.Handle("GET "+base+"/versions", e.authorize(e.handleVersions))
	mux.Handle("GET "+versionRoot, e.authorize(e.handleVersionDetails))
	mux.Handle("GET "+versionRoot+"/", e.authorize(e.handleVersionDetails))

	mux.Handle("GET "+versionRoot+"/locations", e.authorize(e.handleLocations))
	mux.Handle("GET "+versionRoot+"/locations/", e.authorize(e.handleLocations))
	mux.Handle("GET "+versionRoot+"/tariffs", e.authorize(e.handleTariffs))
	mux.Handle("GET "+versionRoot+"/tariffs/", e.authorize(e.handleTariffs))

	mux.Handle("POST "+versionRoot+"/commands/START_SESSION", e.authorize(e.handleStartSession))
	mux.Handle("POST "+versionRoot+"/commands/STOP_SESSION", e.authorize(e.handleStopSession))
	mux.Handle("POST "+versionRoot+"/commands/UNLOCK_CONNECTOR", e.authorize(e.handleUnlockConnector))

	// The chaos knobs of the emulators, reachable by hand (curl) as well as
	// programmatically. They are not part of OCPI.
	mux.HandleFunc("POST "+base+"/admin/scenario", e.handleScenario)
}

// nextSessionID mints the CPO's own id for a session (e.g. VLT-3).
func (e *Emulator) nextSessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	return e.profile.PartyID + "-" + strconv.Itoa(e.seq)
}

// nextCDRID mints the CPO's own id for a CDR.
func (e *Emulator) nextCDRID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cdrSeq++
	return e.profile.PartyID + "-" + store.NewID("cdr")
}

// authorize wraps a handler so it only runs for a request presenting the
// emulator's token_b, the same way a real CPO guards its own endpoints. Everything the emulator serves needs it — versions included.
func (e *Emulator) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || token != e.profile.TokenB {
			ocpi.Fail(w, http.StatusUnauthorized, ocpi.StatusUnauthorized, "Unknown token.")
			return
		}
		next(w, r)
	}
}

// bearerToken parses the OCPI authorization scheme — `Token <token>`.
func bearerToken(header string) (string, bool) {
	const prefix = "Token "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if decoded, err := base64.StdEncoding.DecodeString(token); err == nil {
		token = string(decoded)
	}
	return token, token != ""
}

// send makes one outbound call to ChargeNet authenticated with token_c. target is an absolute URL (a command's response_url) or a path the
// emulator's origin is prefixed to. It returns the HTTP status and body; a
// non-2xx is not an error — the caller decides — but a transport failure is.
func (e *Emulator) send(ctx context.Context, method, target string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}

	url := target
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		url = e.origin + target
	}
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Token "+e.profile.TokenC)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, raw, nil
}

// push sends one best-effort push and logs the outcome. The emulator is the CPO:
// a push it cannot deliver is ChargeNet's problem to notice, not something it
// retries here.
func (e *Emulator) push(ctx context.Context, what, method, target string, body any) {
	status, raw, err := e.send(ctx, method, target, body)
	if err != nil {
		log.Printf("emulator[%s]: %s %s: %v", e.profile.Slug, method, target, err)
		return
	}
	if status/100 != 2 {
		log.Printf("emulator[%s]: %s %s answered %d: %s", e.profile.Slug, method, target, status, strings.TrimSpace(string(raw)))
		return
	}
	log.Printf("emulator[%s]: pushed %s (%s)", e.profile.Slug, what, method)
}

// Close stops the heartbeat, cancels every running session and waits for the
// goroutines to finish. It is idempotent.
func (e *Emulator) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()

	e.cancel()
	e.wg.Wait()
}

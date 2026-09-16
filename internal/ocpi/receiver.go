package ocpi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// TelemetrySink is where the pushes about a running charge reach ChargeNet's
// session orchestrator.
//
// The events are the very [gateway.Telemetry] the simulator emits, so a CPO's
// session push and a simulated tick travel the same path: telemetry
// persistence, SSE fan-out and settlement need no OCPI-specific branch. The
// OCPI gateway ([Gateway], OCPI receivers step 7) implements it; tests record into a fake.
type TelemetrySink interface {
	Publish(ev gateway.Telemetry)
}

// publish hands one event to the orchestrator, if one is wired. Until the OCPI gateway
// switches the platform onto OCPI, ChargeNet runs the local simulator and no
// sink exists; the push is then accepted but changes nothing but the rows the
// receiver itself wrote.
func (s *Server) publish(ev gateway.Telemetry) {
	if s.Sink == nil {
		log.Printf("ocpi: no session sink wired, dropping telemetry for %s", ev.SessionID)
		return
	}
	s.Sink.Publish(ev)
}

// maxPushBody caps a push body. OCPI documents are small and a partner is not
// trusted to send a bounded one.
const maxPushBody = 1 << 20

// pushed is one decoded push: the object, and the exact bytes it arrived as.
// The bytes are what the CDR receiver stores, so a CPO's document keeps the
// fields ChargeNet does not model.
type pushed[T any] struct {
	Object T
	Raw    json.RawMessage
}

// decodePush reads one pushed object into `out`.
//
// A module push carries the object bare (what the spec's examples show), and
// some implementations wrap it in the response envelope instead; both are
// accepted, because refusing the enveloped form would reject a push that means
// exactly the same thing. Anything else is a 400 with OCPI 2000.
func decodePush[T any](w http.ResponseWriter, r *http.Request, what string) (*pushed[T], bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPushBody))
	if err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "The request body could not be read.")
		return nil, false
	}

	raw := json.RawMessage(body)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelopeIsObject(envelope.Data) {
		raw = envelope.Data
	}

	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "The "+what+" could not be parsed.")
		return nil, false
	}
	return &pushed[T]{Object: *out, Raw: raw}, true
}

// objectPath returns the path segments a module URL adds to the module itself:
// `{country_code}/{party_id}/{object_id}` for a push, nothing for
// the module root, or more for a URL ChargeNet does not serve.
func objectPath(r *http.Request, module string) []string {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, VersionURLPath+"/"+module), "/")
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
}

// pushPath reads the identifiers a module push carries — `{country_code}`
// `{party_id}` `{object_id}`, plus the `{evse_uid}` the locations module's
// EVSE-level PATCH appends — and refuses a push that names a party other than
// the one the token belongs to.
//
// It answers the request itself and reports false when it did: the module root
// is a call ChargeNet does not serve (3001), and a URL of any other shape is not
// a module path at all (2000 with a 404).
func (s *Server) pushPath(w http.ResponseWriter, r *http.Request, module string) ([]string, bool) {
	parts := objectPath(r, module)
	switch {
	case len(parts) == 0:
		s.notServed(w, module)
		return nil, false
	case len(parts) < 3 || len(parts) > 4:
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
		return nil, false
	}

	cpo := CPOFrom(r.Context())
	if cpo == nil {
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "No partner on the request.")
		return nil, false
	}
	if !partyMatches(cpo, parts[0], parts[1]) {
		Fail(w, http.StatusForbidden, StatusUnauthorized,
			"That party does not belong to the token this request was made with.")
		return nil, false
	}
	return parts, true
}

// partyMatches reports whether a push's `{country_code}/{party_id}` is the
// partner's own.
func partyMatches(cpo *store.CpoAccount, countryCode, partyID string) bool {
	return strings.EqualFold(countryCode, cpo.CountryCode) && strings.EqualFold(partyID, cpo.PartyID)
}

// notServed answers a call ChargeNet's OCPI surface does not implement. In every
// module ChargeNet is the RECEIVER and does not serve CPO reads. 3001 is the
// spec's "this server cannot process this request" code.
func (s *Server) notServed(w http.ResponseWriter, module string) {
	Fail(w, http.StatusNotImplemented, StatusUnableToUseAPI,
		"ChargeNet receives "+module+" pushes; it does not serve that call.")
}

// failStore answers a repository failure. A row that is not there is the CPO's
// problem — the push names something ChargeNet does not have — and anything else
// is ours.
func (s *Server) failStore(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		Fail(w, http.StatusNotFound, StatusClientError, "ChargeNet does not know that "+what+".")
		return
	}
	log.Printf("ocpi: %s %s: %v", r.Method, r.URL.Path, err)
	Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "ChargeNet could not store the push.")
}

// envelopeIsObject reports whether raw looks like a JSON object, which is how
// decodePush tells a wrapped payload from a bare one.
func envelopeIsObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

package ocpi

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// handleSessions serves the sessions module: a CPO pushes what it
// knows about a charge it is running for ChargeNet.
//
// A push carries one snapshot — the energy so far, and whatever else the CPO
// reports — and naming the session is all the CPO has to do: the receiver looks
// our session up, links the two ids and hands the numbers to the orchestrator,
// which is where every other session event in the platform comes from.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	parts, ok := s.pushPath(w, r, ModuleSessions)
	if !ok {
		return
	}
	if len(parts) != 3 {
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		s.putSession(w, r, parts[2])
	default:
		s.notServed(w, ModuleSessions)
	}
}

// putSession ingests one Session push.
func (s *Server) putSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	cpo := CPOFrom(r.Context())
	push, ok := decodePush[Session](w, r, "session")
	if !ok {
		return
	}
	if push.Object.ID != "" && push.Object.ID != sessionID {
		Fail(w, http.StatusBadRequest, StatusClientError,
			"The session id in the body is not the one in the URL.")
		return
	}

	// Either id may be all we know: a CPO echoes the authorization reference it
	// was given when the charge started, and from then on uses its own id.
	sess, err := s.Store.SessionByOCPI(r.Context(), cpo.ID, sessionID, push.Object.AuthorizationReference)
	if err != nil {
		s.failStore(w, r, "session", err)
		return
	}
	if push.Object.KWh < 0 || (push.Object.Currency != "" && push.Object.Currency != sess.OriginalCurrency) ||
		(push.Object.EVSEUID != "" && CPOEVSEUID(push.Object.EVSEUID) != CPOEVSEUID(sess.EVSEUID)) {
		Fail(w, 400, StatusClientError, "Session energy, currency or EVSE does not match the accepted charge.")
		return
	}
	// The first push is where ChargeNet learns the CPO's name for the session,
	// which is what every later push and the CDR are looked up by. An id already
	// recorded is never replaced: the first partner to name a session owns that
	// name, so a second one cannot make the row its own (store.LinkOCPISession
	// does the update conditionally, so two pushes racing here cannot cross).
	if err := s.Store.LinkOCPISession(r.Context(), cpo.ID, sess.ID, sessionID); err != nil {
		s.failStore(w, r, "session", err)
		return
	}

	ev, carry := sessionTelemetry(*sess, push.Object)
	if carry {
		s.publish(ev)
	}

	push.Object.ID = sessionID
	OK(w, http.StatusOK, push.Object)
}

// sessionTelemetry turns one Session push into the gateway event ChargeNet's
// orchestrator understands.
//
// OCPI describes a session with a single status, and the orchestrator's events
// are either a live snapshot or a terminal one: ACTIVE is a CHARGING snapshot,
// COMPLETED ends the charge, INVALID ends it as a failure. PENDING and RESERVED
// say nothing about a charge we are already running and are accepted without an
// event — the push was understood, there is simply nothing to change.
func sessionTelemetry(sess store.Session, body Session) (gateway.Telemetry, bool) {
	ev := gateway.Telemetry{SessionID: sess.ID, State: sessionState(sess, body)}
	switch strings.ToUpper(strings.TrimSpace(body.Status)) {
	case SessionActive:
		ev.Status = gateway.StatusCharging
		return ev, true
	case SessionCompleted:
		ev.Status = gateway.StatusCompleted
		ev.Reason = simulation.ReasonRemoteStopped
		return ev, true
	case SessionInvalid:
		ev.Status = gateway.StatusFailed
		ev.Reason = simulation.ReasonFault
		return ev, true
	case SessionPending, sessionReserved:
		log.Printf("ocpi: session %s is %s at the CPO; ChargeNet keeps the status it has",
			sess.ID, body.Status)
		return gateway.Telemetry{}, false
	default:
		log.Printf("ocpi: session %s reports the status %q; ChargeNet keeps the status it has",
			sess.ID, body.Status)
		return gateway.Telemetry{}, false
	}
}

// sessionReserved is the one OCPI session status the constants above do not
// name: a reservation becomes a session only when it starts.
const sessionReserved = "RESERVED"

// sessionState reads the numbers a Session push carries.
//
// It starts from what ChargeNet already shows and lets the push overwrite it,
// because OCPI carries no live figures at all: `kwh` is the one number the spec
// always has, `total_cost` is optional, and how fast the charge is running or
// what the battery is at are ChargeNet extensions. CPOs that omit them leave the
// driver's last known values intact.
func sessionState(sess store.Session, body Session) simulation.State {
	st := simulation.State{
		BatterySoC:      sess.BatterySoC,
		KWhDelivered:    body.KWh,
		DurationSeconds: sess.DurationSeconds,
		CurrentPowerKW:  sess.CurrentPowerKW,
		Cost:            sess.TotalCost,
	}
	if body.TotalCost != nil {
		st.Cost = body.TotalCost.ExclVAT
	}

	if ext := body.ChargeNet; ext != nil {
		if ext.BatterySoC > 0 {
			st.BatterySoC = ext.BatterySoC
		}
		if ext.CurrentPowerKW > 0 {
			st.CurrentPowerKW = ext.CurrentPowerKW
		}
		if seconds := int(ext.DurationSeconds); seconds > 0 {
			st.DurationSeconds = seconds
			return st
		}
	}

	// No simulated clock to read: how long the charge
	// has run is the CPO's own start and end dates.
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(body.StartDateTime))
	if err != nil {
		return st
	}
	end := time.Now()
	if raw := strings.TrimSpace(body.EndDateTime); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return st
		}
		end = parsed
	}
	if seconds := int(end.Sub(start).Seconds()); seconds > 0 {
		st.DurationSeconds = seconds
	}
	return st
}

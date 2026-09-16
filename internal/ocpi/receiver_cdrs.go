package ocpi

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// secondsPerHour is the unit OCPI states `total_time` in (the spec: "Total time
// of the session in hours"), and the conversion between it and the seconds
// ChargeNet keeps on a session — the inverse of the division
// [sessions.BuildCDR] does when it writes a document.
const secondsPerHour = 3600

// handleCDRs serves the cdrs module: a CPO posts the record of a
// charge it has finished billing, and can read it back at the URL ChargeNet
// answers with.
//
// CDR posts carry the CPO's id in the body. Read URLs use ChargeNet's eMSP party
// identity. The token and partner-scoped lookup keep CPO records isolated.
// Other calls to this module return 3001.
func (s *Server) handleCDRs(w http.ResponseWriter, r *http.Request) {
	parts := objectPath(r, ModuleCDRs)
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodPost:
			s.postCDR(w, r, "")
		default:
			s.notServed(w, ModuleCDRs)
		}
		return
	}

	if r.Method == http.MethodGet {
		if len(parts) != 3 {
			Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
			return
		}
		if !strings.EqualFold(parts[0], EMSPCountryCode) || !strings.EqualFold(parts[1], EMSPPartyID) {
			Fail(w, http.StatusForbidden, StatusUnauthorized,
				"That party does not belong to the token this request was made with.")
			return
		}
		s.getCDR(w, r, parts[2])
		return
	}

	push, ok := s.pushPath(w, r, ModuleCDRs)
	if !ok {
		return
	}
	if len(push) != 3 {
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.postCDR(w, r, push[2])
	case http.MethodGet:
		s.getCDR(w, r, push[2])
	default:
		s.notServed(w, ModuleCDRs)
	}
}

// postCDR ingests one CDR and settles its session.
//
// This is where a charge becomes money. The CDR may arrive before or after the
// CPO's final Session push, so the receiver handles both: a session still live
// is settled here and now, and one the CPO had already completed is left to that
// settlement and only has its document replaced. Either way
// [internal/store.SettleSession] is what commits the money — it locks the row
// and refuses to settle a session twice — and ChargeNet's tariff, not the CPO's
// document, is what the driver is billed: the document is the CPO's record of
// the charge, and the `chargenet` block ChargeNet adds to it is the platform's
// own share.
func (s *Server) postCDR(w http.ResponseWriter, r *http.Request, urlCDRID string) {
	cpo := CPOFrom(r.Context())
	push, ok := decodePush[CDR](w, r, "cdr")
	if !ok {
		return
	}
	body := push.Object
	if body.ID != "" && urlCDRID != "" && body.ID != urlCDRID {
		Fail(w, http.StatusBadRequest, StatusClientError,
			"The CDR id in the body is not the one in the URL.")
		return
	}
	// A CDR posts its party in the body alone, so that is where it is checked.
	if !partyMatches(cpo, body.CountryCode, body.PartyID) {
		Fail(w, http.StatusForbidden, StatusUnauthorized,
			"That party does not belong to the token this request was made with.")
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		Fail(w, http.StatusBadRequest, StatusClientError, "The CDR does not name the session it bills.")
		return
	}

	sess, err := s.Store.SessionByOCPI(r.Context(), cpo.ID, body.SessionID, "")
	if err != nil {
		s.failStore(w, r, "session", err)
		return
	}
	if body.TotalEnergy < 0 || body.TotalTime < 0 || (body.Currency != "" && body.Currency != sess.OriginalCurrency) ||
		(body.CDRLocation.EVSEUID != "" && CPOEVSEUID(body.CDRLocation.EVSEUID) != CPOEVSEUID(sess.EVSEUID)) {
		Fail(w, 400, StatusClientError, "CDR energy, currency or EVSE does not match the accepted charge.")
		return
	}

	// `total_time` arrives in hours (see [secondsPerHour]) while the session
	// keeps seconds, and the CPO's `total_cost` is the driver's bill only as
	// ChargeNet's own tariff computes it (the store settles from its own rows).
	state := store.SessionTelemetry{
		KWhDelivered:    body.TotalEnergy,
		DurationSeconds: int(math.Round(body.TotalTime * secondsPerHour)),
		BatterySoC:      sess.BatterySoC,
		CurrentPowerKW:  sess.CurrentPowerKW,
		TotalCost:       body.TotalCost.BeforeTaxes,
	}
	settled := !sess.Terminal()
	if settled {
		committed, err := s.Store.SettleSession(r.Context(), store.SettleInput{
			SessionID:      sess.ID,
			Status:         store.StatusCompleted,
			Reason:         simulation.ReasonRemoteStopped,
			State:          state,
			FallbackFeePct: s.Cfg.PlatformFeePct,
			BuildCDR:       sessions.BuildCDR,
		})
		if err != nil {
			s.failStore(w, r, "session", err)
			return
		}
		// The terminal event is the driver's receipt, so it carries what the
		// settlement just billed: the `total_cost` of the CPO's document is the
		// CPO's own number, and ChargeNet bills its own tariff.
		state.TotalCost = committed.TotalCost
	}

	// The CPO's document is what gets stored, with ChargeNet's revenue block
	// carried over from the ledger the settlement just committed.
	ledger, err := s.Store.CDRDocumentFor(r.Context(), sess.ID)
	if err != nil {
		s.failStore(w, r, "cdr", err)
		return
	}
	if err := s.Store.UpdateCDRDocument(r.Context(), sess.ID, cdrWithRevenue(push.Raw, ledger)); err != nil {
		s.failStore(w, r, "cdr", err)
		return
	}
	log.Printf("ocpi: partner=%s stored the cdr of session %s (%.3f kWh)", cpo.ID, sess.ID, body.TotalEnergy)

	if settled {
		// Settled here, so the orchestrator has to hear the charge is over:
		// this is the terminal event its driver's stream is waiting for, and
		// the one that frees the connector in the hub.
		s.publish(gateway.Telemetry{
			SessionID: sess.ID,
			Status:    gateway.StatusCompleted,
			Reason:    simulation.ReasonRemoteStopped,
			State: simulation.State{
				BatterySoC:      state.BatterySoC,
				KWhDelivered:    state.KWhDelivered,
				DurationSeconds: state.DurationSeconds,
				CurrentPowerKW:  state.CurrentPowerKW,
				Cost:            state.TotalCost,
			},
		})
	}
	cdrID := body.ID
	if cdrID == "" {
		cdrID = urlCDRID
	}
	w.Header().Set("Location", s.cdrURL(cdrID))
	OK(w, http.StatusCreated, body)
}

// getCDR answers for one CDR the CPO knows by its own id, which is what the
// `Location` header of a push points at. The URL carried ChargeNet's party, so
// it is the token — and the partner the lookup is scoped to — that decides whose
// record this is.
func (s *Server) getCDR(w http.ResponseWriter, r *http.Request, cdrID string) {
	cpo := CPOFrom(r.Context())
	doc, err := s.Store.OCPICDRDocumentFor(r.Context(), cpo.ID, cdrID)
	if err != nil {
		s.failStore(w, r, "cdr", err)
		return
	}
	OK(w, http.StatusOK, json.RawMessage(doc))
}

// cdrURL is where a CPO reads back a CDR it posted: the module path, with
// ChargeNet's own party and the CPO's id for the record.
func (s *Server) cdrURL(cdrID string) string {
	return s.absolute(VersionURLPath + "/" + ModuleCDRs +
		"/" + EMSPCountryCode + "/" + EMSPPartyID + "/" + cdrID)
}

// cdrWithRevenue adds the settled `chargenet` revenue block to the CPO document.
//
// Everything else in the stored document is the CPO's own: it is the CPO's
// record of the charge, so whatever ChargeNet does not model has to survive the
// trip through this platform.
func cdrWithRevenue(cpoDoc, ledgerDoc []byte) []byte {
	var cpo, ledger map[string]any
	if err := json.Unmarshal(cpoDoc, &cpo); err != nil {
		return cpoDoc
	}
	if err := json.Unmarshal(ledgerDoc, &ledger); err != nil {
		return cpoDoc
	}
	revenue, ok := ledger["chargenet"]
	if !ok {
		return cpoDoc
	}
	cpo["chargenet"] = revenue
	merged, err := json.Marshal(cpo)
	if err != nil {
		return cpoDoc
	}
	return merged
}

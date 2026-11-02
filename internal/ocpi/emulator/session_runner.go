package emulator

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chargenet/chargenet-backend/internal/money"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// maxSessionSeconds is the simulated wall-clock a session may run before the
// emulator ends it itself, matching the platform's four-hour timeout.
const maxSessionSeconds = 4 * 60 * 60

// session is one charge the emulator is running.
type session struct {
	// id is the CPO's own session id (e.g. VLT-3): every Session push and the
	// CDR name it.
	id string
	// ref is ChargeNet's session id, carried as `authorization_reference` so
	// ChargeNet can match the first push it sees.
	ref string
	// locationID is the CPO's own location id.
	locationID string
	// evseUID is ChargeNet's EVSE uid (what the command named); cpoEVSEUID is
	// the CPO's own EVSE uid, without the socket suffix ChargeNet may add.
	evseUID    string
	cpoEVSEUID string
	// connectorID is the OCPI connector id inside the EVSE.
	connectorID string

	connector store.Connector
	station   *store.Station
	token     ocpi.Token

	params simulation.Params
	state  simulation.State

	started  time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

// startRequest is the body of `POST /2.2.1/commands/START_SESSION`.
type startRequest struct {
	ChargeNet              *emulatorChargeParams `json:"chargenet,omitempty"`
	ResponseURL            string                `json:"response_url"`
	Token                  ocpi.Token            `json:"token"`
	LocationID             string                `json:"location_id"`
	EVSEUID                string                `json:"evse_uid"`
	ConnectorID            string                `json:"connector_id"`
	AuthorizationReference string                `json:"authorization_reference"`
}

// stopRequest is the body of `POST /2.2.1/commands/STOP_SESSION`.
type stopRequest struct {
	ResponseURL string `json:"response_url"`
	SessionID   string `json:"session_id"`
}

// unlockRequest is the body of `POST /2.2.1/commands/UNLOCK_CONNECTOR`.
type unlockRequest struct {
	ResponseURL string `json:"response_url"`
	LocationID  string `json:"location_id"`
	EVSEUID     string `json:"evse_uid"`
	ConnectorID string `json:"connector_id"`
}

// addSession registers a running session so a later command can find it.
func (e *Emulator) addSession(s *session) {
	e.mu.Lock()
	e.sessions[s.id] = s
	if s.ref != "" {
		e.byRef[s.ref] = s
	}
	e.mu.Unlock()
}

// forgetSession drops a finished session.
func (e *Emulator) forgetSession(s *session) {
	e.mu.Lock()
	delete(e.sessions, s.id)
	if s.ref != "" {
		delete(e.byRef, s.ref)
	}
	e.mu.Unlock()
}

// sessionByID finds a running session by the CPO's own id.
func (e *Emulator) sessionByID(id string) *session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[id]
}

// stop asks a running session to end. Exactly one stop wins.
func (e *Emulator) stop(s *session) {
	s.stopOnce.Do(func() { close(s.stop) })
}

// handleStartSession serves POST /2.2.1/commands/START_SESSION.
// It answers ACCEPTED at once and runs the charge in the background, exactly as
// a real CPO does: the driver learns the charge is running from the Session
// pushes that follow, not from this call.
func (e *Emulator) handleStartSession(w http.ResponseWriter, r *http.Request) {
	if e.Scenario().Offline {
		ocpi.Fail(w, http.StatusServiceUnavailable, ocpi.StatusUnableToUseAPI, "The charge point is offline.")
		return
	}
	var req startRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		ocpi.Fail(w, http.StatusBadRequest, ocpi.StatusClientError, "The command could not be parsed.")
		return
	}
	if e.takeReject() {
		ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: ocpi.CommandRejected, Message: "The connector is not available."})
		return
	}

	ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: ocpi.CommandAccepted})

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runSession(e.runCtx, req)
	}()
}

// handleStopSession serves POST /2.2.1/commands/STOP_SESSION. It answers
// ACCEPTED and wakes the running session, which pushes its final state and CDR.
func (e *Emulator) handleStopSession(w http.ResponseWriter, r *http.Request) {
	if e.Scenario().Offline {
		ocpi.Fail(w, http.StatusServiceUnavailable, ocpi.StatusUnableToUseAPI, "The charge point is offline.")
		return
	}
	var req stopRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		ocpi.Fail(w, http.StatusBadRequest, ocpi.StatusClientError, "The command could not be parsed.")
		return
	}
	s := e.sessionByID(strings.TrimSpace(req.SessionID))
	if s == nil {
		ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: ocpi.CommandRejected, Message: "Unknown session."})
		return
	}

	ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: ocpi.CommandAccepted})
	e.stop(s)
	e.postCommandResult(e.runCtx, req.ResponseURL, ocpi.CommandStopSession, ocpi.CommandAccepted)
}

// handleUnlockConnector serves POST /2.2.1/commands/UNLOCK_CONNECTOR: it frees
// the cable by putting the EVSE back to AVAILABLE.
func (e *Emulator) handleUnlockConnector(w http.ResponseWriter, r *http.Request) {
	if e.Scenario().Offline {
		ocpi.Fail(w, http.StatusServiceUnavailable, ocpi.StatusUnableToUseAPI, "The charge point is offline.")
		return
	}
	var req unlockRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		ocpi.Fail(w, http.StatusBadRequest, ocpi.StatusClientError, "The command could not be parsed.")
		return
	}

	ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: ocpi.CommandAccepted})

	evse := ocpi.CPOEVSEUID(strings.TrimSpace(req.EVSEUID))
	if evse != "" {
		e.setStatus(evse, store.StatusAvailable)
		e.patchEVSE(e.runCtx, strings.TrimSpace(req.LocationID), evse, store.StatusAvailable)
	}
	e.postCommandResult(e.runCtx, req.ResponseURL, ocpi.CommandUnlockConnector, ocpi.CommandAccepted)
}

// postCommandResult posts a command's asynchronous result to the response_url
// ChargeNet supplied. A missing URL is a no-op, so a
// hand-run curl without one still works.
func (e *Emulator) postCommandResult(ctx context.Context, responseURL, command, result string) {
	if strings.TrimSpace(responseURL) == "" {
		return
	}
	e.push(ctx, command+" result", http.MethodPost, responseURL, ocpi.CommandResponse{Result: result})
}

// runSession runs one charge for its whole lifetime: the EVSE
// goes CHARGING, the Session is pushed every tick, and when the charge ends —
// the driver's stop, the target reached, the timeout or a fault — the emulator
// pushes the final Session and, unless it faulted, the CDR, and frees the EVSE.
func (e *Emulator) runSession(ctx context.Context, req startRequest) {
	connector, station, ok := e.connectorFor(ctx, req.EVSEUID)
	if !ok {
		e.postCommandResult(ctx, req.ResponseURL, ocpi.CommandStartSession, ocpi.CommandRejected)
		return
	}

	s := &session{
		id:          e.nextSessionID(),
		ref:         strings.TrimSpace(req.AuthorizationReference),
		locationID:  strings.TrimSpace(req.LocationID),
		evseUID:     strings.TrimSpace(req.EVSEUID),
		cpoEVSEUID:  ocpi.CPOEVSEUID(req.EVSEUID),
		connectorID: strings.TrimSpace(req.ConnectorID),
		connector:   *connector,
		station:     station,
		token:       req.Token,
		started:     time.Now().UTC(),
		stop:        make(chan struct{}),
	}
	if s.ref != "" {
		s.id = e.profile.PartyID + "-" + strings.TrimPrefix(s.ref, "ses_")
	}
	s.params = simulation.Params{
		BatteryKWh:      simulation.FallbackBatteryKWh,
		ConnectorMaxKW:  connector.MaxPowerKW,
		VehicleAcceptKW: simulation.VehicleAcceptKW(connector.PowerType),
		TariffPerKWh:    connector.TariffPerKWh,
		TargetSoC:       0,
	}
	s.state = simulation.State{BatterySoC: simulation.StartSoC(e.rng)}
	if req.ChargeNet != nil {
		s.params = req.ChargeNet.Params
		s.connector.TariffPerKWh = s.params.TariffPerKWh
		if req.ChargeNet.Currency != "" {
			s.connector.Currency = req.ChargeNet.Currency
		}
		if req.ChargeNet.Resume != nil {
			s.state = *req.ChargeNet.Resume
		} else {
			s.state.BatterySoC = req.ChargeNet.StartSoC
		}
	}
	// Repeated remote starts with the same authorization reference resume the
	// existing simulation instead of creating a second operator session.
	e.mu.Lock()
	existing := e.byRef[s.ref]
	e.mu.Unlock()
	if existing != nil {
		e.postCommandResult(ctx, req.ResponseURL, ocpi.CommandStartSession, ocpi.CommandAccepted)
		return
	}
	e.addSession(s)
	defer e.forgetSession(s)

	// The command's own asynchronous result, and the EVSE going CHARGING.
	e.postCommandResult(ctx, req.ResponseURL, ocpi.CommandStartSession, ocpi.CommandAccepted)
	e.setStatus(s.cpoEVSEUID, store.StatusCharging)
	e.patchEVSE(ctx, s.locationID, s.cpoEVSEUID, store.StatusCharging)

	// The fault knob of the emulators.
	var faultAt time.Time
	if seconds := e.Scenario().FaultAfterSeconds; seconds > 0 {
		faultAt = time.Now().Add(time.Duration(seconds) * time.Second)
	}

	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			e.finish(ctx, s, store.StatusCompleted)
			return
		case <-ticker.C:
			next, done, _ := simulation.Step(s.state, s.params, float64(e.scale), e.rng)
			s.state = next
			original, _, err := money.Charge(next.KWhDelivered, strconv.FormatFloat(s.params.TariffPerKWh, 'f', 6, 64), "1")
			if err == nil {
				s.state.Cost = money.Float(original)
			}

			switch {
			case !faultAt.IsZero() && time.Now().After(faultAt):
				e.finish(ctx, s, store.StatusFailed)
				return
			case done,
				s.state.DurationSeconds >= maxSessionSeconds:
				e.finish(ctx, s, store.StatusCompleted)
				return
			default:
				e.pushSession(ctx, s, ocpi.SessionActive, "")
			}
		}
	}
}

// finish ends a running session: the EVSE goes back to
// AVAILABLE, the final Session is pushed — COMPLETED, or INVALID for a fault —
// and, unless it faulted, the CDR follows.
func (e *Emulator) finish(ctx context.Context, s *session, status string) {
	e.setStatus(s.cpoEVSEUID, store.StatusAvailable)
	e.patchEVSE(ctx, s.locationID, s.cpoEVSEUID, store.StatusAvailable)

	ocpiStatus := ocpi.SessionCompleted
	if status == store.StatusFailed {
		ocpiStatus = ocpi.SessionInvalid
	}
	e.pushSession(ctx, s, ocpiStatus, nowRFC3339())

	if status != store.StatusFailed {
		e.pushCDR(ctx, s)
	}
}

// cdrTariffID is the tariff id the emulator stamps on its CDRs. ChargeNet bills
// from its own tariff (the connector's stored price), so this is only the CPO's
// label for the price component it reports.
const cdrTariffID = "energy"

// pushSession pushes one Session snapshot to ChargeNet. The
// non-OCPI `chargenet` block carries the live figures OCPI has no field for, so
// the driver's charging screen shows them.
func (e *Emulator) pushSession(ctx context.Context, s *session, status, endDateTime string) {
	body := ocpi.Session{
		CountryCode:            e.profile.CountryCode,
		PartyID:                e.profile.PartyID,
		ID:                     s.id,
		StartDateTime:          s.started.Format(time.RFC3339),
		EndDateTime:            endDateTime,
		KWh:                    s.state.KWhDelivered,
		CDRToken:               s.token,
		AuthMethod:             ocpi.AuthMethodCommand,
		AuthorizationReference: s.ref,
		LocationID:             s.locationID,
		EVSEUID:                s.evseUID,
		ConnectorID:            s.connectorID,
		Currency:               s.connector.Currency,
		Status:                 status,
		TotalCost:              &ocpi.Price{ExclVAT: s.state.Cost},
		ChargeNet: &ocpi.SessionTelemetry{
			CurrentPowerKW:  s.state.CurrentPowerKW,
			BatterySoC:      s.state.BatterySoC,
			DurationSeconds: float64(s.state.DurationSeconds),
		},
		LastUpdated: nowRFC3339(),
	}
	path := fmt.Sprintf("/ocpi/emsp/2.3.0/%s/%s/%s/%s",
		ocpi.ModuleSessions, e.profile.CountryCode, e.profile.PartyID, url.PathEscape(s.id))
	e.push(ctx, "session", http.MethodPut, path, body)
}

// patchEVSE pushes one EVSE status change to ChargeNet's locations receiver
// : the EVSE-level PATCH the spec defines.
func (e *Emulator) patchEVSE(ctx context.Context, locationID, evseUID, status string) {
	if strings.TrimSpace(locationID) == "" || strings.TrimSpace(evseUID) == "" {
		return
	}
	path := fmt.Sprintf("/ocpi/emsp/2.3.0/%s/%s/%s/%s/%s",
		ocpi.ModuleLocations, e.profile.CountryCode, e.profile.PartyID,
		url.PathEscape(locationID), url.PathEscape(evseUID))
	body := map[string]any{"uid": evseUID, "status": ocpiEVSEStatus(status)}
	e.push(ctx, "evse "+status, http.MethodPatch, path, body)
}

// pushCDR posts the record of a finished charge to ChargeNet.
// The platform bills the driver from its own tariff, so this document is the
// CPO's account of the charge — and what ChargeNet stores verbatim.
func (e *Emulator) pushCDR(ctx context.Context, s *session) {
	end := time.Now().UTC()
	hours := float64(s.state.DurationSeconds) / secondsPerHour

	loc := ocpi.CDRLocation{
		Country:            ocpi.CountryAlpha3(s.station.CountryCode),
		CountryCode:        e.profile.CountryCode,
		PartyID:            e.profile.PartyID,
		ID:                 s.locationID,
		Address:            "-",
		City:               "-",
		Coordinates:        ocpi.GeoLocation{Latitude: "0.000000", Longitude: "0.000000"},
		EVSEUID:            s.cpoEVSEUID,
		ConnectorID:        s.connectorID,
		ConnectorStandard:  ocpiStandard(s.connector.Standard),
		ConnectorFormat:    s.connector.Format,
		ConnectorPowerType: wirePowerType(s.connector.PowerType),
	}
	if s.station != nil {
		loc.ID = s.locationID
		loc.Name = s.station.Name
		loc.Address = s.station.Address
		loc.City = s.station.City
		loc.Coordinates = ocpi.GeoLocation{
			Latitude:  strconv.FormatFloat(s.station.Latitude, 'f', 6, 64),
			Longitude: strconv.FormatFloat(s.station.Longitude, 'f', 6, 64),
		}
	}

	cdr := ocpi.CDR{
		CountryCode:   e.profile.CountryCode,
		PartyID:       e.profile.PartyID,
		ID:            e.nextCDRID(),
		StartDateTime: s.started.Format(time.RFC3339),
		EndDateTime:   end.Format(time.RFC3339),
		SessionID:     s.id,
		CDRToken:      s.token,
		AuthMethod:    ocpi.AuthMethodCommand,
		CDRLocation:   loc,
		Currency:      s.connector.Currency,
		Tariffs: []ocpi.CDRTariff{{
			ID:          firstNonBlank(valueOrEmpty(s.connector.TariffID), cdrTariffID),
			CountryCode: e.profile.CountryCode, PartyID: e.profile.PartyID, LastUpdated: nowRFC3339(), TaxIncluded: "N/A",
			Currency: s.connector.Currency,
			Elements: []ocpi.TariffElement{{
				PriceComponents: []ocpi.PriceComponent{{Type: "ENERGY", Price: s.connector.TariffPerKWh, StepSize: 1}},
			}},
		}},
		ChargingPeriods: []ocpi.ChargingPeriod{{
			StartDateTime: s.started.Format(time.RFC3339),
			Dimensions: []ocpi.Dimension{
				{Type: "ENERGY", Volume: s.state.KWhDelivered},
				{Type: "TIME", Volume: hours},
			},
		}},
		TotalCost:   ocpi.Price{ExclVAT: s.state.Cost},
		TotalEnergy: s.state.KWhDelivered,
		TotalTime:   hours,
		LastUpdated: nowRFC3339(),
	}
	e.push(ctx, "cdr", http.MethodPost, "/ocpi/emsp/2.3.0/"+ocpi.ModuleCDRs, cdr)
}

// secondsPerHour converts a duration to the hours OCPI's `total_time` is in.
const secondsPerHour = 3600

// connectorFor resolves the connector a START_SESSION names from the store. The
// emulator runs the same physics ChargeNet would, so it needs the connector's
// rated power and tariff.
func (e *Emulator) connectorFor(ctx context.Context, evseUID string) (*store.Connector, *store.Station, bool) {
	if e.store == nil || strings.TrimSpace(evseUID) == "" {
		return nil, nil, false
	}
	st, conn, err := e.store.GetEVSE(ctx, evseUID)
	if err != nil {
		log.Printf("emulator[%s]: EVSE %s: %v", e.profile.Slug, evseUID, err)
		return nil, nil, false
	}
	return conn, st, true
}

// firstNonBlank returns the first of its arguments that is not blank.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type emulatorChargeParams struct {
	Currency string            `json:"currency"`
	Params   simulation.Params `json:"params"`
	StartSoC float64           `json:"start_soc"`
	Resume   *simulation.State `json:"resume,omitempty"`
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

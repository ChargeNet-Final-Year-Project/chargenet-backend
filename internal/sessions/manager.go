package sessions

import (
	"context"
	"errors"
	"github.com/chargenet/chargenet-backend/internal/money"
	"log"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// Orchestrator tuning.
const (
	// MinWalletRequired is the wallet floor for starting a charge when the
	// configuration does not override MIN_START_BALANCE.
	MinWalletRequired = 10.00
	// DefaultPlatformFeePct is the fallback fee split when neither the CPO row
	// nor the configuration sets one.
	DefaultPlatformFeePct = 5.0
	// DefaultTelemetryInterval is how often live numbers are written to
	// Postgres; the simulator ticks every second, the database does not need to.
	DefaultTelemetryInterval = 2 * time.Second
	// MaxSessionSeconds force-stops a session after four simulated hours.
	MaxSessionSeconds = 4 * 3600
	// DefaultTargetSoC is used when the client sends no targetSoc.
	DefaultTargetSoC = 80
	// stopWait is how long Stop waits for the settlement it triggered.
	stopWait = 3 * time.Second
	// settleTimeout bounds the settlement transaction itself. It is detached
	// from the request context so a disconnect cannot abort a settle half way.
	settleTimeout = 15 * time.Second
)

// liveSession is the orchestrator's in-memory view of a running charge: the
// last durable row (so an SSE frame is a complete DTO without a read per tick),
// the wallet balance the auto-stop guard watches, and the throttling clock.
type liveSession struct {
	base        store.Session
	balance     float64
	lastPersist time.Time
}

// reasonedStopper is implemented by gateways that can label a stop with its
// reason (the local simulator does). An OCPI CPO decides its own reasons, so
// the orchestrator keeps its own record either way.
type reasonedStopper interface {
	StopWithReason(ctx context.Context, sessionID, reason string) error
}

// Manager is the session manager session orchestrator.
type Manager struct {
	store *store.Store
	gw    gateway.CPOGateway
	hub   *Hub

	minBalance        float64
	feePct            float64
	telemetryInterval time.Duration
	maxSeconds        int

	mu      sync.Mutex
	rng     *rand.Rand
	live    map[string]*liveSession
	intents map[string]string // sessionID → reason the orchestrator chose
}

// Option tunes a [Manager] (tests use it to shrink the timings).
type Option func(*Manager)

// WithTelemetryInterval changes how often live numbers reach the database.
func WithTelemetryInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.telemetryInterval = d
		}
	}
}

// WithMaxSessionSeconds changes the simulated-duration ceiling.
func WithMaxSessionSeconds(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.maxSeconds = n
		}
	}
}

// WithRand makes the start-of-charge SoC repeatable across runs.
func WithRand(r *rand.Rand) Option {
	return func(m *Manager) {
		if r != nil {
			m.rng = r
		}
	}
}

// NewManager wires the orchestrator to its two seams. minStartBalance and the
// platform fee split come from the process configuration
// with the documented defaults when unset.
func NewManager(st *store.Store, gw gateway.CPOGateway, cfg config.Config, opts ...Option) *Manager {
	m := &Manager{
		store:             st,
		gw:                gw,
		hub:               NewHub(),
		minBalance:        cfg.MinStartBalance,
		feePct:            cfg.PlatformFeePct,
		telemetryInterval: DefaultTelemetryInterval,
		maxSeconds:        MaxSessionSeconds,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
		live:              map[string]*liveSession{},
		intents:           map[string]string{},
	}
	if m.minBalance <= 0 {
		m.minBalance = MinWalletRequired
	}
	if m.feePct <= 0 {
		m.feePct = DefaultPlatformFeePct
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// StartRequest is the decoded `POST /sessions/start` body.
type StartRequest struct {
	QuoteID   string
	EVSEUID   string
	VehicleID string
	TargetSoC float64
}

// Start runs ordered validation and opens the charge.
//
// The order matters: the driver's existing session, the EVSE, the connector's
// availability, the wallet floor and the vehicle/connector compatibility are
// checked before anything is written, so the client always gets the most
// relevant 4xx. The PENDING row plus its partial unique indexes are what make
// two simultaneous starts safe — the checks above only make the failure
// readable.
func (m *Manager) Start(ctx context.Context, userID string, req StartRequest) (*store.Session, error) {
	evse := strings.ToUpper(strings.TrimSpace(req.EVSEUID))
	if evse == "" {
		return nil, Errorf(CodeValidation, "evseUid is required.")
	}
	target := req.TargetSoC
	if target == 0 {
		target = DefaultTargetSoC
	}
	if target < 1 || target > 100 {
		return nil, Errorf(CodeValidation, "targetSoc must be between 1 and 100.")
	}

	active, err := m.store.GetActiveSession(ctx, userID)
	switch {
	case err == nil:
		return nil, Errorf(CodeSessionAlreadyActive, "You already have a session in progress (%s).", active.ID)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	station, conn, err := m.store.GetEVSE(ctx, evse)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(CodeEVSENotFound, "We couldn't find that charger.")
	}
	if errors.Is(err, store.ErrCpoInactive) {
		// Suspended partners are unavailable to drivers.
		return nil, Errorf(CodeConnectorUnavailable, "That charger is not available right now.")
	}
	if err != nil {
		return nil, err
	}
	if conn.Status != store.StatusAvailable {
		return nil, Errorf(CodeConnectorUnavailable,
			"That charger is %s right now.", strings.ToLower(conn.Status))
	}

	vehicle, err := m.resolveVehicle(ctx, userID, req.VehicleID)
	if err != nil {
		return nil, err
	}
	if vehicle != nil && !Compatible(vehicle.ConnectorType, conn.Standard) {
		return nil, Errorf(CodeIncompatibleConnector,
			"A %s vehicle cannot use a %s connector.", vehicle.ConnectorType, conn.Standard)
	}

	wallet, err := m.store.GetWallet(ctx, userID)
	if err != nil {
		return nil, err
	}
	if wallet.AvailableBalance < m.minBalance {
		required, balance := m.minBalance, wallet.Balance
		return nil, &Error{
			Code:     CodeInsufficientBalance,
			Message:  "A minimum wallet balance is required to start charging.",
			Required: &required,
			Balance:  &balance,
		}
	}

	if req.QuoteID == "" {
		return nil, store.ErrQuoteRequired
	}
	params := m.params(*conn, vehicle, target)
	startSoC := m.drawStartSoC()

	sess, err := m.store.InsertPendingSession(ctx, store.PendingSessionInput{
		UserID:         userID,
		QuoteID:        req.QuoteID,
		VehicleID:      vehicleID(vehicle),
		CPOID:          station.CpoID,
		StationID:      station.ID,
		ConnectorID:    conn.ID,
		EVSEUID:        conn.EVSEUID,
		StartSoC:       startSoC,
		CurrentPowerKW: simulation.PowerKW(startSoC, simulation.CapKW(conn.MaxPowerKW, params.VehicleAcceptKW), nil),
		TargetSoC:      target,
		TariffPerKWh:   conn.TariffPerKWh,
	})
	switch {
	case errors.Is(err, store.ErrActiveSession):
		return nil, Errorf(CodeSessionAlreadyActive, "You already have a session in progress.")
	case errors.Is(err, store.ErrConnectorBusy):
		return nil, Errorf(CodeConnectorUnavailable, "That charger is in use right now.")
	case err != nil:
		return nil, err
	}

	params.TariffPerKWh = sess.TariffPerKWh
	params.MaxEnergyKWh = budgetEnergy(*sess)
	if err := m.gw.StartSession(ctx, gateway.StartCmd{
		SessionID:      sess.ID,
		TariffCurrency: sess.OriginalCurrency,
		EVSEUID:        sess.EVSEUID,
		Params:         params,
		StartSoC:       startSoC,
	}); err != nil {
		// The charge never started: bill nothing, just free the connector.
		if ferr := m.store.FailSession(context.WithoutCancel(ctx), sess.ID, simulation.ReasonFault); ferr != nil {
			log.Printf("sessions: fail unstarted %s: %v", sess.ID, ferr)
		}
		return nil, fromGateway(err)
	}

	if err := m.store.ActivateSession(ctx, sess.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		// ErrNotFound here means the session is already terminal — the charge
		// ended on the gateway's very first tick. The terminal event is being
		// settled by the consumer goroutine, so the caller still gets the row.
		return nil, err
	}
	running, err := m.store.SessionByID(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	if running.Active() {
		m.track(running, sessionBudget(*running, wallet.Balance))
	}
	return running, nil
}

// Hub exposes the SSE fan-out to the API layer.
func (m *Manager) Hub() *Hub { return m.hub }

// resolveVehicle picks the vehicle a charge runs against: the one the client
// named, or the driver's default when it named none. Charging without a stored
// vehicle uses fallback battery figures.
func (m *Manager) resolveVehicle(ctx context.Context, userID, vehicleID string) (*store.Vehicle, error) {
	if strings.TrimSpace(vehicleID) == "" {
		return m.store.DefaultVehicle(ctx, userID)
	}
	v, err := m.store.GetVehicle(ctx, userID, vehicleID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(CodeVehicleNotFound, "We couldn't find that vehicle.")
	}
	return v, err
}

// vehicleID renders the optional vehicle column.
func vehicleID(v *store.Vehicle) string {
	if v == nil {
		return ""
	}
	return v.ID
}

// Params builds the physics one session charges with: the pack
// size comes from the driver's vehicle, or the 60 kWh fallback when there is
// none; acceptance is the AC/DC fallback because the vehicles table records no
// per-car acceptance.
func (m *Manager) params(conn store.Connector, vehicle *store.Vehicle, targetSoC float64) simulation.Params {
	p := simulation.Params{
		BatteryKWh:      simulation.FallbackBatteryKWh,
		ConnectorMaxKW:  conn.MaxPowerKW,
		VehicleAcceptKW: simulation.VehicleAcceptKW(conn.PowerType),
		TariffPerKWh:    conn.TariffPerKWh,
		TargetSoC:       targetSoC,
	}
	if vehicle != nil && vehicle.BatteryCapacity > 0 {
		p.BatteryKWh = vehicle.BatteryCapacity
	}
	return p
}

// drawStartSoC draws the 15–35 % a session begins at, rounded to one
// decimal so the mobile shows the same number the row stores.
func (m *Manager) drawStartSoC() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return round1(simulation.StartSoC(m.rng))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Compatible reports whether a vehicle of connector type vehicleType may charge
// on connectorStandard: the types must match exactly, except
// that a CCS2 car may also take an AC Type 2 socket. An unknown type on either
// side is treated as compatible so missing catalogue data does not block a charge.
func Compatible(vehicleType, connectorStandard string) bool {
	v := strings.ToUpper(strings.TrimSpace(vehicleType))
	c := strings.ToUpper(strings.TrimSpace(connectorStandard))
	switch {
	case v == "" || c == "":
		return true
	case v == c:
		return true
	default:
		return v == store.StandardCCS2 && c == store.StandardTYPE2
	}
}

// track registers a running session and the balance its guard watches.
func (m *Manager) track(sess *store.Session, balance float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live[sess.ID] = &liveSession{base: *sess, balance: balance, lastPersist: time.Now()}
}

// telemetryOf reads the persistable snapshot out of a stored row (used when a
// session has to be settled from what the database already holds).
func telemetryOf(s store.Session) store.SessionTelemetry {
	return store.SessionTelemetry{
		DurationSeconds: s.DurationSeconds,
		KWhDelivered:    s.KWhDelivered,
		CurrentPowerKW:  s.CurrentPowerKW,
		BatterySoC:      s.BatterySoC,
		TotalCost:       s.TotalCost,
	}
}

// telemetryFrom maps one gateway tick onto the persistable snapshot.
func telemetryFrom(st simulation.State) store.SessionTelemetry {
	return store.SessionTelemetry{
		DurationSeconds: st.DurationSeconds,
		KWhDelivered:    st.KWhDelivered,
		CurrentPowerKW:  st.CurrentPowerKW,
		BatterySoC:      st.BatterySoC,
		TotalCost:       st.Cost,
	}
}

// applyTelemetry overlays live numbers onto a cached row so an SSE frame carries
// the complete ChargingSession DTO.
func applyTelemetry(sess *store.Session, t store.SessionTelemetry) {
	sess.Status = store.StatusCharging
	sess.DurationSeconds = t.DurationSeconds
	sess.KWhDelivered = t.KWhDelivered
	sess.CurrentPowerKW = t.CurrentPowerKW
	sess.BatterySoC = t.BatterySoC
	original, converted, err := money.Charge(t.KWhDelivered, sess.TariffDecimal, sess.ExchangeRate)
	if err == nil {
		sess.OriginalCost = money.Float(original)
		sess.TotalCost = money.Float(converted)
	}
}

// Run resumes whatever survived a restart and then
// consumes gateway telemetry until ctx is cancelled.
//
// The subscription is taken before the recovery pass so no frame emitted while
// re-arming a session can be lost, and the consumer keeps running in the
// background after Run returns — the caller hands it the process context and
// carries on starting the HTTP server.
func (m *Manager) Run(ctx context.Context) {
	events := m.gw.Subscribe()
	m.recover(ctx)
	go m.consume(ctx, events)
}

// consume is the single reader of the gateway's event channel: settlement,
// persistence and SSE fan-out all happen here, in order, for every session.
func (m *Manager) consume(ctx context.Context, events <-chan gateway.Telemetry) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return // the gateway closed: nothing more will arrive
			}
			m.handle(ctx, ev)
		}
	}
}

// recover re-arms the sessions that were mid-charge when the process stopped.
//
// A PENDING row means the backend died between opening the session and the CPO
// accepting it: no energy flowed, so it is failed and its connector released. A
// CHARGING row is handed back to the gateway with its stored state as the
// starting point, so the charge continues where it left off.
func (m *Manager) recover(ctx context.Context) {
	rows, err := m.store.ListResumableSessions(ctx)
	if err != nil {
		log.Printf("sessions: recovery scan failed: %v", err)
		return
	}
	for _, r := range rows {
		sess := r.Session
		if sess.SettlementRequestedStatus != nil {
			reason := simulation.ReasonRemoteStopped
			if sess.SettlementRequestedReason != nil {
				reason = *sess.SettlementRequestedReason
			}
			m.finish(ctx, sess.ID, *sess.SettlementRequestedStatus, reason, telemetryOf(*sess))
			continue
		}
		if sess.Status == store.StatusPending {
			if err := m.store.FailSession(ctx, sess.ID, simulation.ReasonFault); err != nil {
				log.Printf("sessions: fail interrupted pending %s: %v", sess.ID, err)
			}
			continue
		}
		if r.Connector == nil {
			log.Printf("sessions: session %s has no connector row, failing it", sess.ID)
			m.finish(ctx, sess.ID, store.StatusFailed, simulation.ReasonFault, telemetryOf(*sess))
			continue
		}
		if sess.DurationSeconds >= m.maxSeconds {
			m.setIntent(sess.ID, simulation.ReasonTimeout)
			m.finish(ctx, sess.ID, store.StatusCompleted, simulation.ReasonTimeout, telemetryOf(*sess))
			continue
		}

		params := m.params(*r.Connector, r.Vehicle, sess.TargetSoC)
		params.TariffPerKWh = sess.TariffPerKWh
		params.MaxEnergyKWh = budgetEnergy(*sess)
		cmd := gateway.StartCmd{
			TariffCurrency: sess.OriginalCurrency,
			SessionID:      sess.ID,
			EVSEUID:        sess.EVSEUID,
			Params:         params,
			Resume: &simulation.State{
				BatterySoC:      sess.BatterySoC,
				KWhDelivered:    sess.KWhDelivered,
				DurationSeconds: sess.DurationSeconds,
				CurrentPowerKW:  sess.CurrentPowerKW,
				Cost:            sess.OriginalCost,
			},
		}
		if err := m.gw.StartSession(ctx, cmd); err != nil {
			log.Printf("sessions: could not resume %s: %v", sess.ID, err)
			m.finish(ctx, sess.ID, store.StatusFailed, simulation.ReasonFault, telemetryOf(*sess))
			continue
		}
		balance := 0.0
		if wallet, werr := m.store.GetWallet(ctx, sess.UserID); werr == nil {
			balance = wallet.Balance
		}
		m.track(sess, sessionBudget(*sess, balance))
		log.Printf("sessions: resumed %s on %s after restart", sess.ID, sess.ConnectorID)
	}
}

// handle routes one gateway event: terminal events settle, live ticks persist,
// publish and then face the two guards only the orchestrator can evaluate — the
// simulated clock and the driver's wallet.
func (m *Manager) handle(ctx context.Context, ev gateway.Telemetry) {
	if ev.Terminal() {
		status := store.StatusCompleted
		if ev.Status == gateway.StatusFailed {
			status = store.StatusFailed
		}
		reason := m.takeIntent(ev.SessionID)
		if reason == "" {
			reason = ev.Reason
		}
		if reason == "" {
			if status == store.StatusFailed {
				reason = simulation.ReasonFault
			} else {
				reason = simulation.ReasonTargetReached
			}
		}
		m.finish(ctx, ev.SessionID, status, reason, telemetryFrom(ev.State))
		return
	}

	m.persist(ctx, ev)
	m.publish(ev)

	if ev.State.DurationSeconds >= m.maxSeconds {
		m.stopFor(ctx, ev.SessionID, simulation.ReasonTimeout)
		return
	}
	m.mu.Lock()
	live, ok := m.live[ev.SessionID]
	exhausted := ok && live.base.TotalCost >= live.balance
	m.mu.Unlock()
	if exhausted {
		m.stopFor(ctx, ev.SessionID, simulation.ReasonBalanceExhausted)
	}
}

// persist writes the live numbers at most once per telemetry interval. The
// in-memory copy is refreshed on every tick, so an SSE frame is always current
// even when the database write is throttled.
func (m *Manager) persist(ctx context.Context, ev gateway.Telemetry) {
	state := telemetryFrom(ev.State)

	m.mu.Lock()
	live, ok := m.live[ev.SessionID]
	due := false
	if ok {
		applyTelemetry(&live.base, state)
		if time.Since(live.lastPersist) >= m.telemetryInterval {
			live.lastPersist = time.Now()
			due = true
		}
	}
	m.mu.Unlock()
	if !due {
		return
	}
	if err := m.store.UpdateSessionTelemetry(ctx, ev.SessionID, state); err != nil {
		log.Printf("sessions: telemetry %s: %v", ev.SessionID, err)
	}
}

// publish fans the cached snapshot of a tracked session out to its stream.
func (m *Manager) publish(ev gateway.Telemetry) {
	m.mu.Lock()
	live, ok := m.live[ev.SessionID]
	var frame Frame
	if ok {
		frame = Frame{Session: live.base}
	}
	m.mu.Unlock()
	if ok {
		m.hub.Publish(ev.SessionID, frame)
	}
}

// balanceOf is the balance a running session is spending against.
func (m *Manager) balanceOf(sessionID string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	live, ok := m.live[sessionID]
	if !ok {
		return 0, false
	}
	return live.balance, true
}

// setIntent records why the orchestrator decided to stop a session. The first
// reason wins: a driver tapping stop after the wallet guard fired must not
// rewrite the history of why the charge ended.
func (m *Manager) setIntent(sessionID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.intents[sessionID]; !exists {
		m.intents[sessionID] = reason
	}
}

// takeIntent returns and clears the recorded reason.
func (m *Manager) takeIntent(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	reason := m.intents[sessionID]
	delete(m.intents, sessionID)
	return reason
}

// clearLive forgets a finished session (its row is in the database now).
func (m *Manager) clearLive(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, sessionID)
	delete(m.intents, sessionID)
}

// stopFor asks the gateway to stop a session, labelling the stop with the
// orchestrator's reason where the gateway supports it (the local simulator
// does; an OCPI CPO decides for itself and the recorded intent is what the
// settlement writes).
func (m *Manager) stopFor(ctx context.Context, sessionID, reason string) {
	m.setIntent(sessionID, reason)
	if rs, ok := m.gw.(reasonedStopper); ok {
		err := rs.StopWithReason(ctx, sessionID, reason)
		if err != nil && !gateway.IsCode(err, gateway.CodeSessionNotFound) {
			log.Printf("sessions: stop %s (%s): %v", sessionID, reason, err)
		}
		return
	}
	if err := m.gw.StopSession(ctx, sessionID); err != nil && !gateway.IsCode(err, gateway.CodeSessionNotFound) {
		log.Printf("sessions: stop %s (%s): %v", sessionID, reason, err)
	}
}

// settle runs the settlement transaction with this manager's CDR builder and
// fee fallback. The timeout is detached from the caller's context: a client
// that disconnects mid-stop must not leave a finished charge unsettled.
func (m *Manager) settle(ctx context.Context, in store.SettleInput) (*store.Session, error) {
	in.BuildCDR = BuildCDR
	if in.FallbackFeePct <= 0 {
		in.FallbackFeePct = m.feePct
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return m.store.SettleSession(sctx, in)
}

// finish settles a finished session and closes its stream. On a settlement
// failure the row is re-read so the client still receives a terminal frame
// instead of waiting forever; the reconciliation that repairs the ledger can
// then see the orphaned session.
func (m *Manager) finish(ctx context.Context, sessionID, status, reason string, state store.SessionTelemetry) *store.Session {
	durableCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	markErr := m.store.RequestSettlement(durableCtx, sessionID, status, reason, state)
	cancel()
	if markErr != nil {
		log.Printf("sessions: retaining final meter %s: %v", sessionID, markErr)
	}
	var sess *store.Session
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		sess, err = m.settle(ctx, store.SettleInput{SessionID: sessionID, Status: status, Reason: reason, State: state})
		if err == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if err != nil {
		log.Printf("sessions: settle %s failed: %v", sessionID, err)
		if row, rerr := m.store.SessionByID(ctx, sessionID); rerr == nil {
			m.hub.Complete(sessionID, Frame{Session: *row})
		} else {
			m.hub.Complete(sessionID, Frame{})
		}
		return nil
	}
	m.clearLive(sessionID)
	m.hub.Complete(sessionID, Frame{Session: *sess})
	return sess
}

// Stop ends a charge on the driver's request. It is idempotent: a
// session that already finished is returned unchanged, so a double tap or a
// retry after a dropped response settles nothing twice.
func (m *Manager) Stop(ctx context.Context, userID, sessionID string) (*store.Session, error) {
	sess, err := m.store.GetSession(ctx, userID, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(CodeSessionNotFound, "We couldn't find that session.")
	}
	if err != nil {
		return nil, err
	}
	if sess.Terminal() {
		return sess, nil
	}

	m.setIntent(sessionID, simulation.ReasonUser)
	err = m.gw.StopSession(ctx, sessionID)
	switch {
	case err == nil:
		return m.waitFor(ctx, sessionID, sess), nil
	case gateway.IsCode(err, gateway.CodeSessionNotFound):
		// A missing remote session is not terminal evidence. A terminal event
		// may have settled the row while the stop command was in flight; only
		// that durable result can authorize an idempotent successful response.
		row, rerr := m.store.GetSession(ctx, userID, sessionID)
		if rerr != nil {
			return nil, rerr
		}
		if row.Terminal() {
			return row, nil
		}
		// Keep tracking and reserving funds until authoritative telemetry
		// arrives. The local session exists, so this is a confirmation failure
		// (502), not a driver-facing session-not-found response (404).
		return nil, Errorf(CodeCPOUnreachable, "The operator could not confirm that charging stopped. Your session remains active; retry or wait for an update.")
	default:
		return nil, fromGateway(err)
	}
}

// waitFor waits for the settlement a stop triggered. StopSession returns once
// the gateway has published the terminal event, but the settlement itself runs
// on the consumer goroutine; this polls for a short, bounded while and falls
// back to the last row it saw, so an HTTP request can never hang on it.
func (m *Manager) waitFor(ctx context.Context, sessionID string, last *store.Session) *store.Session {
	deadline := time.Now().Add(stopWait)
	for {
		if row, err := m.store.SessionByID(ctx, sessionID); err == nil {
			last = row
			if row.Terminal() {
				return row
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Active returns the driver's live session.
func (m *Manager) Active(ctx context.Context, userID string) (*store.Session, error) {
	sess, err := m.store.GetActiveSession(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(CodeNoActiveSession, "You have no session in progress.")
	}
	return sess, err
}

// Get returns one of the driver's own sessions.
func (m *Manager) Get(ctx context.Context, userID, sessionID string) (*store.Session, error) {
	sess, err := m.store.GetSession(ctx, userID, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Errorf(CodeSessionNotFound, "We couldn't find that session.")
	}
	return sess, err
}

// History paging bounds ).
const (
	HistoryDefaultLimit = 50
	HistoryMaxLimit     = 100
)

// History pages the driver's sessions, newest first, clamping the page size.
func (m *Manager) History(ctx context.Context, userID string, limit, offset int) ([]store.Session, error) {
	switch {
	case limit <= 0:
		limit = HistoryDefaultLimit
	case limit > HistoryMaxLimit:
		limit = HistoryMaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return m.store.ListSessionHistory(ctx, userID, limit, offset)
}

// Subscribe opens the live frame stream of a session the driver owns and returns
// the row to seed it with.
//
// Subscribe before reading the row so ticks remain buffered during the lookup.
// The owner-scoped lookup hides sessions belonging to another driver.
func (m *Manager) Subscribe(ctx context.Context, userID, sessionID string) (*Subscription, *store.Session, error) {
	sub := m.hub.Subscribe(sessionID)
	sess, err := m.Get(ctx, userID, sessionID)
	if err != nil {
		sub.Close()
		return nil, nil, err
	}
	return sub, sess, nil
}

// Shutdown closes every open stream so SSE handlers finish their responses.
// Sessions still charging are re-armed from the database on the next boot.
func (m *Manager) Shutdown() { m.hub.Close() }

func sessionBudget(s store.Session, fallback float64) float64 {
	if s.QuoteID != nil {
		return s.ReservedAmount
	}
	return fallback
}
func budgetEnergy(s store.Session) float64 {
	if s.QuoteID == nil || s.ReservedAmount <= 0 {
		return 0
	}
	return money.EnergyBudget(strconv.FormatFloat(s.ReservedAmount, 'f', 2, 64), s.TariffDecimal, s.ExchangeRate)
}

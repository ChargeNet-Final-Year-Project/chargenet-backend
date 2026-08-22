package gateway

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/chargenet/chargenet-backend/internal/simulation"
)

var _ CPOGateway = (*LocalGateway)(nil)

// DefaultTimeScale is the simulated seconds per real second when none is
// configured; it matches SIM_TIME_SCALE's default.
const DefaultTimeScale = 10

// DefaultTick is the real-time cadence every session is stepped at.
const DefaultTick = time.Second

// subscriberBuffer is how far behind a subscriber may fall before its frames are
// dropped instead of stalling the simulator.
const subscriberBuffer = 64

// LocalGateway runs one simulation goroutine per session when GATEWAY=local.
// It shares charging physics with the OCPI emulators and publishes Telemetry.
type LocalGateway struct {
	scale int
	tick  time.Duration

	mu       sync.Mutex
	base     *rand.Rand // seed source for per-session jitter, guarded by mu
	sessions map[string]*localSession
	subs     map[chan Telemetry]struct{}
	closed   bool
}

// Option configures a [LocalGateway] at construction.
type Option func(*LocalGateway)

// WithTick overrides the real-time tick. Tests use a few milliseconds so a
// "30-minute" session finishes instantly; production keeps [DefaultTick].
func WithTick(d time.Duration) Option {
	return func(g *LocalGateway) {
		if d > 0 {
			g.tick = d
		}
	}
}

// WithRand replaces the jitter source so a run is repeatable. Each session
// derives its own stream from it, which keeps one session's jitter from moving
// another's numbers.
func WithRand(r *rand.Rand) Option {
	return func(g *LocalGateway) {
		if r != nil {
			g.base = r
		}
	}
}

// NewLocalGateway builds a gateway where one real second is `scale` simulated
// seconds. A scale below 1 falls back to [DefaultTimeScale], because a session
// that takes an hour of wall clock is useless for a demo.
func NewLocalGateway(scale int, opts ...Option) *LocalGateway {
	if scale < 1 {
		scale = DefaultTimeScale
	}
	g := &LocalGateway{
		scale:    scale,
		tick:     DefaultTick,
		base:     rand.New(rand.NewSource(time.Now().UnixNano())),
		sessions: make(map[string]*localSession),
		subs:     make(map[chan Telemetry]struct{}),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// localSession is the bookkeeping for one running session.
type localSession struct {
	cancel context.CancelFunc
	// stop carries the reason the caller chose; buffered so exactly one stop
	// wins and a second caller just waits on done.
	stop chan string
	// fault is triggered by InjectFault (CPO sandbox / emulator error).
	fault chan struct{}
	// done is closed once the terminal event has been published.
	done chan struct{}
	rng  *rand.Rand
}

// StartSession registers the session, publishes its first snapshot and starts
// stepping it. It returns as soon as the session is running (ACCEPTED): the
// caller does not wait for the first tick.
//
// ctx is honoured before the session starts, but the session itself runs on its
// own context — it must outlive the HTTP request that created it.
func (g *LocalGateway) StartSession(ctx context.Context, cmd StartCmd) error {
	if err := ctx.Err(); err != nil {
		return Errorf(CodeCPOUnreachable, "gateway unavailable: %v", err)
	}
	if cmd.SessionID == "" || cmd.EVSEUID == "" {
		return Errorf(CodeValidation, "start session needs a session id and an evse uid")
	}
	if cmd.Params.BatteryKWh <= 0 {
		return Errorf(CodeValidation, "session %s has no battery capacity to charge", cmd.SessionID)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	s := &localSession{
		cancel: cancel,
		stop:   make(chan string, 1),
		fault:  make(chan struct{}, 1),
		done:   make(chan struct{}),
		rng:    rand.New(rand.NewSource(g.nextSeed())),
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		cancel()
		return Errorf(CodeCPOUnreachable, "gateway is closed")
	}
	if _, exists := g.sessions[cmd.SessionID]; exists {
		g.mu.Unlock()
		cancel()
		// Reject duplicate starts because two runners could settle the session twice.
		return Errorf(CodeSessionAlreadyActive, "session %s is already running", cmd.SessionID)
	}
	g.sessions[cmd.SessionID] = s
	g.mu.Unlock()

	state := startState(cmd, s.rng)

	// The first snapshot is published before StartSession returns, so the
	// caller's DTO and the first SSE frame agree, and a client that subscribes
	// straight after POST /sessions/start immediately has something to draw.
	g.publish(Telemetry{SessionID: cmd.SessionID, State: state, Status: StatusCharging})

	go g.run(runCtx, cmd, s, state)
	return nil
}

// startState is the session's first snapshot: the state stored before a restart
// when resuming, otherwise a fresh start at the caller's or a random SoC.
func startState(cmd StartCmd, rng *rand.Rand) simulation.State {
	if cmd.Resume != nil {
		return *cmd.Resume
	}
	soc := cmd.StartSoC
	if soc <= 0 {
		soc = simulation.StartSoC(rng)
	}
	return simulation.State{
		BatterySoC:     soc,
		CurrentPowerKW: simulation.PowerKW(soc, simulation.CapKW(cmd.Params.ConnectorMaxKW, cmd.Params.VehicleAcceptKW), nil),
	}
}

// run steps one session until it finishes. The session's own goroutine owns the
// live state, which is why the stop and fault paths can publish a terminal event
// with the latest numbers without any locking of their own.
func (g *LocalGateway) run(ctx context.Context, cmd StartCmd, s *localSession, state simulation.State) {
	defer s.cancel()

	ticker := time.NewTicker(g.tick)
	defer ticker.Stop()

	simDt := float64(g.scale)

	for {
		select {
		case <-ctx.Done():
			// Shutdown (Close): no terminal event. The orchestrator settles
			// whatever the database holds on the next boot, and inventing a USER stop here would settle
			// sessions the driver never stopped.
			g.forget(cmd.SessionID)
			return
		case reason := <-s.stop:
			g.publish(Telemetry{
				SessionID: cmd.SessionID,
				State:     state,
				Status:    StatusCompleted,
				Reason:    reason,
			})
			g.end(cmd.SessionID, s)
			return
		case <-s.fault:
			g.publish(Telemetry{
				SessionID: cmd.SessionID,
				State:     state,
				Status:    StatusFailed,
				Reason:    simulation.ReasonFault,
			})
			g.end(cmd.SessionID, s)
			return
		case <-ticker.C:
		}

		next, done, reason := simulation.Step(state, cmd.Params, simDt, s.rng)
		state = next

		status, eventReason := StatusCharging, ""
		if done {
			status, eventReason = StatusCompleted, reason
		}
		g.publish(Telemetry{SessionID: cmd.SessionID, State: state, Status: status, Reason: eventReason})

		if done {
			g.end(cmd.SessionID, s)
			return
		}
	}
}

// StopSession stops a session because the caller asked.
func (g *LocalGateway) StopSession(ctx context.Context, sessionID string) error {
	return g.StopWithReason(ctx, sessionID, simulation.ReasonUser)
}

// StopWithReason stops a session and labels its terminal event with the caller's
// reason — BALANCE_EXHAUSTED and TIMEOUT are only visible to the orchestrator,
// which watches the wallet and the simulated clock.
//
// It is deliberately not part of [CPOGateway]: an OCPI CPO decides for itself why
// a session ended, so the interface stays reason-free and the session manager records its own
// reason when the transport cannot.
func (g *LocalGateway) StopWithReason(ctx context.Context, sessionID, reason string) error {
	if reason == "" {
		reason = simulation.ReasonUser
	}

	g.mu.Lock()
	s, ok := g.sessions[sessionID]
	g.mu.Unlock()
	if !ok {
		return Errorf(CodeSessionNotFound, "session %s is not running", sessionID)
	}

	select {
	case s.stop <- reason:
	case <-s.done: // already finished; nothing to wait for
	case <-ctx.Done():
		return fmt.Errorf("gateway: stop %s: %w", sessionID, ctx.Err())
	}

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gateway: stop %s: %w", sessionID, ctx.Err())
	}
}

// InjectFault makes the next tick of sessionID fail, the way a CPO sandbox error
// would: the session publishes a terminal FAILED event with reason FAULT
// . Unknown and already-finished sessions report SESSION_NOT_FOUND.
func (g *LocalGateway) InjectFault(sessionID string) error {
	g.mu.Lock()
	s, ok := g.sessions[sessionID]
	g.mu.Unlock()
	if !ok {
		return Errorf(CodeSessionNotFound, "session %s is not running", sessionID)
	}

	select {
	case s.fault <- struct{}{}:
	default: // a fault is already queued for this session
	}
	return nil
}

// UnlockConnector is a no-op here: connector availability lives in Postgres and
// the API layer owns it (store.SetConnectorStatus). The method exists so the OCPI
// gateway can forward UNLOCK_CONNECTOR to the CPO without changing the
// interface the session manager codes against.
func (g *LocalGateway) UnlockConnector(_ context.Context, evseUID string) error {
	if evseUID == "" {
		return Errorf(CodeValidation, "unlock needs an evse uid")
	}
	return nil
}

// Subscribe registers a buffered listener for session events. Slow subscribers
// lose stale frames and can refresh the session over HTTP.
func (g *LocalGateway) Subscribe() <-chan Telemetry {
	ch := make(chan Telemetry, subscriberBuffer)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		close(ch)
		return ch
	}
	g.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe removes a listener. It is an extra method on the concrete gateway
// because [CPOGateway] has no way to leave a subscription; the session manager's SSE hub calls it
// when a request ends. Dropped listener channels are never written to again, so
// they are safe to abandon.
func (g *LocalGateway) Unsubscribe(ch <-chan Telemetry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for c := range g.subs {
		if c == ch {
			delete(g.subs, c)
			return
		}
	}
}

// Close stops every running session and drops every subscriber. It is meant for
// process shutdown: sessions end without a terminal event, so the orchestrator
// settles them from the database on the next boot. Close is idempotent, and a
// subscription taken afterwards returns an already-closed channel.
func (g *LocalGateway) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	live := make([]*localSession, 0, len(g.sessions))
	for _, s := range g.sessions {
		live = append(live, s)
	}
	g.sessions = make(map[string]*localSession)
	g.subs = make(map[chan Telemetry]struct{})
	g.mu.Unlock()

	for _, s := range live {
		s.cancel()
	}
}

// publish fans an event out to every subscriber, skipping full buffers.
func (g *LocalGateway) publish(t Telemetry) {
	g.mu.Lock()
	targets := make([]chan Telemetry, 0, len(g.subs))
	for ch := range g.subs {
		targets = append(targets, ch)
	}
	g.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- t:
		default:
		}
	}
}

// forget drops a finished session so its id can be started again.
func (g *LocalGateway) forget(sessionID string) {
	g.mu.Lock()
	delete(g.sessions, sessionID)
	g.mu.Unlock()
}

// end forgets the session and wakes anyone waiting in StopWithReason. It runs
// after the terminal event has been published, so a caller returning from a stop
// can rely on subscribers having seen the event.
func (g *LocalGateway) end(sessionID string, s *localSession) {
	g.forget(sessionID)
	close(s.done)
}

// nextSeed hands each session its own jitter stream.
func (g *LocalGateway) nextSeed() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.base.Int63()
}

package ocpi

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// Compile-time proofs that the OCPI gateway is the swap-in for both seams it
// stands behind: the orchestrator's transport ([gateway.CPOGateway], the session manager) and the
// receivers' two sinks, so switching the platform from the local simulator
// onto real CPOs is one wiring change and no interface churn.
var (
	_ gateway.CPOGateway = (*Gateway)(nil)
	_ TelemetrySink      = (*Gateway)(nil)
	_ CommandResultSink  = (*Gateway)(nil)
)

// subscriberBuffer is how far behind a subscriber may fall before its frames are
// dropped instead of stalling a CPO's push. It matches the local gateway, and
// for the same reason: telemetry is worthless late, and a caller can re-read the
// session over HTTP.
const subscriberBuffer = 64

// Gateway is ChargeNet's OCPI CPO gateway: it sends the
// commands to a partner over [CommandClient] and fans out the
// telemetry the partner's pushes produce.
//
// It is the real [gateway.CPOGateway]. The receivers hand it what they translate
// (they are [TelemetrySink]) and the asynchronous answers to the commands it sent
// (it is [CommandResultSink]), which is what lets a charge started by command end
// when the CPO says it ended, whether that is a final Session push or a rejected
// command.
type Gateway struct {
	store    *store.Store
	client   *Client
	baseURL  string
	commands *CommandClient

	mu   sync.Mutex
	subs map[chan gateway.Telemetry]struct{}
	// waiters holds the callers still waiting for a session's terminal event,
	// keyed by ChargeNet session id; each waiter is woken exactly once.
	waiters map[string]map[chan struct{}]struct{}
	closed  bool
}

// GatewayOption configures a [Gateway] at construction.
type GatewayOption func(*Gateway)

// WithOCPIClient replaces the HTTP client the gateway calls partners with. Tests
// point it at an httptest server's own client; production keeps the default.
func WithOCPIClient(c *Client) GatewayOption {
	return func(g *Gateway) {
		if c != nil {
			g.client = c
		}
	}
}

// WithBaseURL overrides the origin the `response_url` callbacks are built from.
// It defaults to the configured PUBLIC_BASE_URL; it exists so a test can point
// the callbacks at the receiver it is actually serving.
func WithBaseURL(baseURL string) GatewayOption {
	return func(g *Gateway) {
		if strings.TrimSpace(baseURL) != "" {
			g.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// NewGateway builds the OCPI gateway over a store and the platform config. The
// store is where a session's OCPI addressing lives (store.OCPICommandTarget) and
// cfg.PublicBaseURL is the origin a CPO posts command results back to.
func NewGateway(st *store.Store, cfg config.Config, opts ...GatewayOption) *Gateway {
	g := &Gateway{
		store:   st,
		client:  NewClient(),
		baseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		subs:    make(map[chan gateway.Telemetry]struct{}),
		waiters: make(map[string]map[chan struct{}]struct{}),
	}
	for _, opt := range opts {
		opt(g)
	}
	g.commands = NewCommandClient(g.client, g.baseURL)
	return g
}

// StartSession asks the CPO behind a ChargeNet session to begin charging. It
// returns nil once the CPO has queued the charge (ACCEPTED); the session turns
// CHARGING as the CPO's first Session push arrives, not here.
func (g *Gateway) StartSession(ctx context.Context, cmd gateway.StartCmd) error {
	if strings.TrimSpace(cmd.SessionID) == "" {
		return gateway.Errorf(gateway.CodeValidation, "a start needs a session id")
	}
	target, cpo, err := g.addressing(ctx, cmd.SessionID)
	if err != nil {
		return err
	}
	res, err := g.commands.StartSession(ctx, cpo, target, emulatorExtension(cpo, cmd))
	if err != nil {
		return err
	}
	return startFailure(res)
}

// UnlockConnector asks a CPO to release the cable on one of its EVSEs. Unlike
// the other two commands there is no session to address, so the EVSE uid is the
// whole handle.
func (g *Gateway) UnlockConnector(ctx context.Context, evseUID string) error {
	if strings.TrimSpace(evseUID) == "" {
		return gateway.Errorf(gateway.CodeValidation, "an unlock needs an evse uid")
	}
	target, err := g.store.OCPIUnlockTarget(ctx, evseUID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return gateway.Errorf(gateway.CodeConnectorUnavailable, "ChargeNet knows no connector %s", evseUID)
		}
		return err
	}
	cpo, err := g.cpo(ctx, target.CPOID)
	if err != nil {
		return err
	}
	res, err := g.commands.UnlockConnector(ctx, cpo, target)
	if err != nil {
		return err
	}
	switch res.Result {
	case CommandAccepted:
		return nil
	case CommandRejected, CommandNotSupported, CommandTimeout, "":
		return gateway.Errorf(gateway.CodeConnectorUnavailable, resultReason(res, "the CPO refused the unlock"))
	default:
		return gateway.Errorf(gateway.CodeCPOUnreachable, "the CPO answered the unlock with %q", res.Result)
	}
}

// addressing loads everything a command needs to find its CPO: the session's
// OCPI addressing facts and the partner they belong to.
func (g *Gateway) addressing(ctx context.Context, sessionID string) (*store.OCPICommandTarget, *store.CpoAccount, error) {
	target, err := g.store.OCPICommandTarget(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, gateway.Errorf(gateway.CodeSessionNotFound,
				"ChargeNet knows no session %s", sessionID)
		}
		return nil, nil, err
	}
	cpo, err := g.cpo(ctx, target.CPOID)
	if err != nil {
		return nil, nil, err
	}
	return target, cpo, nil
}

// cpo loads a partner's credentials, or reports it as unreachable: a session
// pointing at a partner ChargeNet cannot name is a call it cannot make.
func (g *Gateway) cpo(ctx context.Context, cpoID string) (*store.CpoAccount, error) {
	cpo, err := g.store.GetCPO(ctx, cpoID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, gateway.Errorf(gateway.CodeCPOUnreachable, "CPO %s is not on file", cpoID)
		}
		return nil, err
	}
	return cpo, nil
}

// startFailure turns the synchronous answer to START_SESSION into the failure
// the session manager expects, or nil when the CPO queued the charge.
//
// REJECTED is a connector problem — the CPO would not start on the EVSE the
// driver chose — while NOT_SUPPORTED, TIMEOUT, an empty result and anything
// unrecognised mean the partner could not be used at all.
func startFailure(res CommandResponse) error {
	switch res.Result {
	case CommandAccepted:
		return nil
	case CommandRejected:
		return gateway.Errorf(gateway.CodeConnectorUnavailable, resultReason(res, "the CPO refused the start"))
	case CommandNotSupported:
		return gateway.Errorf(gateway.CodeCPOUnreachable, resultReason(res, "the CPO cannot start sessions"))
	case CommandTimeout:
		return gateway.Errorf(gateway.CodeCPOUnreachable, resultReason(res, "the CPO did not answer the start in time"))
	case "":
		return gateway.Errorf(gateway.CodeCPOUnreachable, "the CPO answered the start with no result")
	default:
		return gateway.Errorf(gateway.CodeCPOUnreachable, "the CPO answered the start with %q", res.Result)
	}
}

// resultReason is the CPO's own message when it sent one, and a default when it
// did not — a failure a driver reads should never be blank.
func resultReason(res CommandResponse, fallback string) string {
	if strings.TrimSpace(res.Message) != "" {
		return res.Message
	}
	return fallback
}

// StopSession asks the CPO to end a charge and returns once the session has
// ended. The CPO confirms the stop twice — synchronously on the call, and again
// by pushing the session's final state — and either confirmation is the end of
// the wait, because a CPO that posts only one of them must not hang a driver's
// stop.
func (g *Gateway) StopSession(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return gateway.Errorf(gateway.CodeValidation, "a stop needs a session id")
	}
	target, cpo, err := g.addressing(ctx, sessionID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(target.OCPISessionID) == "" {
		// A CPO names a session with its own id the first time it pushes one;
		// until then there is nothing to ask it to stop.
		return gateway.Errorf(gateway.CodeSessionNotFound,
			"the CPO has not named session %s yet, so it cannot be stopped", sessionID)
	}

	// The waiter is registered before the command goes out: a CPO that answers
	// both the call and the push instantly must not slip its event past us.
	done, leave := g.expectEnd(sessionID)
	defer leave()

	res, err := g.commands.StopSession(ctx, cpo, target)
	if err != nil {
		return err
	}
	switch res.Result {
	case CommandAccepted:
	case CommandRejected, CommandNotSupported:
		return gateway.Errorf(gateway.CodeCPOUnreachable, resultReason(res, "the CPO refused the stop"))
	case CommandTimeout:
		return gateway.Errorf(gateway.CodeCPOUnreachable, resultReason(res, "the CPO did not answer the stop in time"))
	case "":
		return gateway.Errorf(gateway.CodeCPOUnreachable, "the CPO answered the stop with no result")
	default:
		return gateway.Errorf(gateway.CodeCPOUnreachable, "the CPO answered the stop with %q", res.Result)
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return gateway.Errorf(gateway.CodeCPOUnreachable,
			"the CPO did not confirm the stop of session %s", sessionID)
	}
}

// expectEnd registers a caller as waiting for one session's terminal event and
// returns the channel it wakes on plus the function that clears the
// registration.
func (g *Gateway) expectEnd(sessionID string) (<-chan struct{}, func()) {
	done := make(chan struct{})

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		close(done)
		return done, func() {}
	}
	if g.waiters[sessionID] == nil {
		g.waiters[sessionID] = make(map[chan struct{}]struct{})
	}
	g.waiters[sessionID][done] = struct{}{}
	g.mu.Unlock()

	return done, func() {
		g.mu.Lock()
		delete(g.waiters[sessionID], done)
		if len(g.waiters[sessionID]) == 0 {
			delete(g.waiters, sessionID)
		}
		g.mu.Unlock()
	}
}

// signalEnd wakes everyone waiting on a session's terminal event. It is safe to
// call for a session nobody is waiting on.
func (g *Gateway) signalEnd(sessionID string) {
	g.mu.Lock()
	for ch := range g.waiters[sessionID] {
		close(ch)
	}
	delete(g.waiters, sessionID)
	g.mu.Unlock()
}

// Publish is the [TelemetrySink] the OCPI receivers translate a CPO's pushes
// into: a live snapshot becomes a CHARGING event and a final
// session a COMPLETED or FAILED one, exactly the events the local simulator
// emits, so the orchestrator's persistence, fan-out and settlement need no
// OCPI-specific branch.
func (g *Gateway) Publish(ev gateway.Telemetry) {
	g.mu.Lock()
	targets := make([]chan gateway.Telemetry, 0, len(g.subs))
	for ch := range g.subs {
		targets = append(targets, ch)
	}
	g.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- ev:
		default: // a subscriber that is whole buffer behind loses the frame
		}
	}
	// A terminal event is what a StopSession caller is waiting for.
	if ev.Terminal() {
		g.signalEnd(ev.SessionID)
	}
}

// CommandResult is the [CommandResultSink] the commands receiver hands a CPO's
// asynchronous answer to.
//
// Only two of the three commands can mean something here. An asynchronous
// START_SESSION that is not ACCEPTED is a charge that will never run: the
// session the session manager opened is ended as FAILED so its connector comes back, which is
// what "the CPO turned it down late" has to look like. A STOP_SESSION answer
// wakes whoever is waiting in StopSession, because the CPO confirming the stop
// is the end of the wait whether or not its final Session push has landed yet.
// UNLOCK_CONNECTOR has no waiter, so its answer changes nothing.
func (g *Gateway) CommandResult(command, uid string, res CommandResponse) {
	switch command {
	case CommandStartSession:
		if res.Result == CommandAccepted {
			return
		}
		g.failPendingStart(uid, resultReason(res, "the CPO did not accept the start"))
	case CommandStopSession:
		g.signalEnd(uid)
	case CommandUnlockConnector:
		// Unlock is answered on the call itself; there is nothing to wait for.
	default:
		// The receiver may forward commands added by a newer OCPI version.
	}
}

// failPendingStart publishes the terminal event for a session whose start the
// CPO turned down after it had accepted the call. The orchestrator settles it,
// which returns the connector, exactly as if the rejection had been synchronous.
// The CPO's own words go to the log, because [gateway.Telemetry] carries a
// reason code a driver's receipt understands, not free text.
func (g *Gateway) failPendingStart(sessionID, reason string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	log.Printf("ocpi: session %s: CPO turned down the start: %s", sessionID, reason)
	g.Publish(gateway.Telemetry{
		SessionID: sessionID,
		Status:    gateway.StatusFailed,
		Reason:    simulation.ReasonFault,
	})
}

// Subscribe buffers CPO events so slow listeners cannot block the CPO's HTTP call.
func (g *Gateway) Subscribe() <-chan gateway.Telemetry {
	ch := make(chan gateway.Telemetry, subscriberBuffer)

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
// because [gateway.CPOGateway] has no way to leave a subscription; the session manager's SSE hub
// calls it when a request ends.
func (g *Gateway) Unsubscribe(ch <-chan gateway.Telemetry) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for c := range g.subs {
		if c == ch {
			delete(g.subs, c)
			return
		}
	}
}

// Close drops every subscriber and wakes every waiting stop. It is meant for
// process shutdown: a session ends without a terminal event, so the
// orchestrator settles it from the database on the next boot. Close is
// idempotent, and a subscription taken afterwards returns an already-closed
// channel.
func (g *Gateway) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	waiters := g.waiters
	g.subs = make(map[chan gateway.Telemetry]struct{})
	g.waiters = make(map[string]map[chan struct{}]struct{})
	g.mu.Unlock()

	for _, set := range waiters {
		for ch := range set {
			close(ch)
		}
	}
}

func emulatorExtension(cpo *store.CpoAccount, cmd gateway.StartCmd) any {
	if !strings.Contains(cpo.BaseURL, "/emulator/") {
		return nil
	}
	return map[string]any{"params": cmd.Params, "currency": cmd.TariffCurrency, "start_soc": cmd.StartSoC, "resume": cmd.Resume}
}

package gateway

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/simulation"
)

// fastGateway builds a gateway whose ticks are near-instant, so a session the
// driver would see as half an hour long finishes inside a test. Sixty simulated
// seconds per 2 ms tick keeps the arithmetic obvious (one tick = one minute).
func fastGateway(t *testing.T) *LocalGateway {
	t.Helper()
	g := NewLocalGateway(60,
		WithTick(2*time.Millisecond),
		WithRand(rand.New(rand.NewSource(7))),
	)
	t.Cleanup(g.Close)
	return g
}

// dcParams is the reference session the reference charging scenario uses.
func dcParams(target float64) simulation.Params {
	return simulation.Params{
		BatteryKWh:      60,
		ConnectorMaxKW:  150,
		VehicleAcceptKW: 150,
		TariffPerKWh:    1.20,
		TargetSoC:       target,
	}
}

// awaitTelemetry reads frames until one matches, or fails the test. Telemetry is
// asynchronous by nature, so every wait is bounded — a hung simulator must fail
// loudly instead of blocking the suite.
func awaitTelemetry(t *testing.T, ch <-chan Telemetry, match func(Telemetry) bool) Telemetry {
	t.Helper()

	deadline := time.After(5 * time.Second)
	var last Telemetry
	seen := false

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("the gateway closed its telemetry channel")
			}
			last, seen = e, true
			if match(e) {
				return e
			}
		case <-deadline:
			if !seen {
				t.Fatal("timed out waiting for telemetry: nothing was published")
			}
			t.Fatalf("timed out waiting for telemetry: last event was %+v", last)
		}
	}
}

func TestLocalGatewayStartStepsAndStopPublishesTerminalEvent(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	const id = "ses_b7_start"
	if err := g.StartSession(ctx, StartCmd{
		SessionID: id,
		EVSEUID:   "VLT-SGB-01",
		Params:    dcParams(80),
		StartSoC:  20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// The first frame is the session's starting snapshot, published before
	// StartSession returned.
	first := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id })
	if first.Status != StatusCharging {
		t.Fatalf("first frame status = %q, want %q", first.Status, StatusCharging)
	}
	if first.State.BatterySoC != 20 || first.State.KWhDelivered != 0 {
		t.Fatalf("first frame state = %+v, want the 20 %% start", first.State)
	}
	if first.State.CurrentPowerKW <= 0 {
		t.Fatalf("first frame power = %v, want the curve's value at 20 %%", first.State.CurrentPowerKW)
	}

	// Then it ticks: at least three more frames, with energy strictly rising.
	kwh := first.State.KWhDelivered
	for i := 0; i < 3; i++ {
		next := awaitTelemetry(t, sub, func(e Telemetry) bool {
			return e.SessionID == id && !e.Terminal()
		})
		if next.State.KWhDelivered <= kwh {
			t.Fatalf("tick %d: energy %v did not rise from %v", i+1, next.State.KWhDelivered, kwh)
		}
		if next.State.BatterySoC <= 20 || next.State.BatterySoC > 80 {
			t.Fatalf("tick %d: SoC = %v, want (20, 80]", i+1, next.State.BatterySoC)
		}
		if want := (i + 1) * 60; next.State.DurationSeconds != want {
			t.Fatalf("tick %d: duration = %d simulated seconds, want %d", i+1, next.State.DurationSeconds, want)
		}
		kwh = next.State.KWhDelivered
	}

	if err := g.StopSession(ctx, id); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// StopSession returns only after the terminal event has been published.
	last := awaitTelemetry(t, sub, func(e Telemetry) bool {
		return e.SessionID == id && e.Terminal()
	})
	if last.Status != StatusCompleted {
		t.Fatalf("terminal status = %q, want %q", last.Status, StatusCompleted)
	}
	if last.Reason != simulation.ReasonUser {
		t.Fatalf("terminal reason = %q, want %q", last.Reason, simulation.ReasonUser)
	}
	if last.State.KWhDelivered < kwh {
		t.Fatalf("terminal energy %v is behind the last tick %v", last.State.KWhDelivered, kwh)
	}
	if want := simulation.Round2(last.State.KWhDelivered * 1.20); last.State.Cost != want {
		t.Fatalf("terminal cost = %v, want round2(kWh × tariff) = %v", last.State.Cost, want)
	}

	// A finished session is forgotten, so a second stop is not a silent success.
	if err := g.StopSession(ctx, id); !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("second StopSession = %v, want %s", err, CodeSessionNotFound)
	}
}

func TestLocalGatewayStopsForTheReasonItWasGiven(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	const id = "ses_b7_balance"
	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-02", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// BALANCE_EXHAUSTED and TIMEOUT are invisible to the gateway, so the
	// orchestrator hands its own reason to StopWithReason.
	if err := g.StopWithReason(ctx, id, simulation.ReasonBalanceExhausted); err != nil {
		t.Fatalf("StopWithReason: %v", err)
	}

	last := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id && e.Terminal() })
	if last.Reason != simulation.ReasonBalanceExhausted {
		t.Fatalf("terminal reason = %q, want %q", last.Reason, simulation.ReasonBalanceExhausted)
	}
}

func TestLocalGatewayFinishesOnItsOwnWhenTheTargetIsReached(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	const id = "ses_b7_target"
	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	// 20 % → 25 % is a couple of ticks at ~150 kW.
	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-03", Params: dcParams(25), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	last := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id && e.Terminal() })
	if last.Status != StatusCompleted || last.Reason != simulation.ReasonTargetReached {
		t.Fatalf("terminal event = %+v, want COMPLETED / %s", last, simulation.ReasonTargetReached)
	}
	if last.State.BatterySoC != 25 {
		t.Fatalf("final SoC = %v, want exactly the 25 %% target", last.State.BatterySoC)
	}

	if err := g.StopSession(ctx, id); !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("StopSession after the target = %v, want %s", err, CodeSessionNotFound)
	}
}

func TestLocalGatewayInjectsFaults(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	const id = "ses_b7_fault"
	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-04", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id })

	// The demo's LAN-fault drill ends the session as failed.
	if err := g.InjectFault(id); err != nil {
		t.Fatalf("InjectFault: %v", err)
	}

	last := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id && e.Terminal() })
	if last.Status != StatusFailed || last.Reason != simulation.ReasonFault {
		t.Fatalf("terminal event = %+v, want FAILED / %s", last, simulation.ReasonFault)
	}

	if err := g.InjectFault("ses_b7_missing"); !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("InjectFault on an unknown session = %v, want %s", err, CodeSessionNotFound)
	}
}

func TestLocalGatewayRejectsUnusableStartCommands(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	cases := []struct {
		name string
		cmd  StartCmd
		code string
	}{
		{"no session id", StartCmd{EVSEUID: "VLT-SGB-01", Params: dcParams(80)}, CodeValidation},
		{"no EVSE", StartCmd{SessionID: "ses_b7_bad", Params: dcParams(80)}, CodeValidation},
		{
			"no battery capacity",
			StartCmd{SessionID: "ses_b7_bad", EVSEUID: "VLT-SGB-01"},
			CodeValidation,
		},
	}
	for _, c := range cases {
		if err := g.StartSession(ctx, c.cmd); !IsCode(err, c.code) {
			t.Errorf("%s: StartSession = %v, want %s", c.name, err, c.code)
		}
	}

	// A valid start still works after the rejections. Its duplicate is rejected.
	if err := g.StartSession(ctx, StartCmd{
		SessionID: "ses_b7_dup", EVSEUID: "VLT-SGB-01", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	err := g.StartSession(ctx, StartCmd{
		SessionID: "ses_b7_dup", EVSEUID: "VLT-SGB-01", Params: dcParams(80), StartSoC: 20,
	})
	if !IsCode(err, CodeSessionAlreadyActive) {
		t.Fatalf("duplicate StartSession = %v, want %s", err, CodeSessionAlreadyActive)
	}

	// Stopping something that was never started is not a silent success either.
	if err := g.StopSession(ctx, "ses_b7_never"); !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("StopSession on an unknown session = %v, want %s", err, CodeSessionNotFound)
	}
}

func TestLocalGatewayResumesFromStoredState(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	const id = "ses_b7_resume"
	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	// Reboot recovery: the orchestrator seeds the simulator with the
	// row it read back from the database instead of a fresh session.
	stored := &simulation.State{BatterySoC: 60, KWhDelivered: 24, Cost: 28.8, DurationSeconds: 900}
	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-05", Params: dcParams(80), Resume: stored,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	first := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id })
	if first.State != *stored {
		t.Fatalf("resumed first frame = %+v, want the stored %+v", first.State, *stored)
	}

	next := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id && !e.Terminal() })
	if next.State.KWhDelivered <= stored.KWhDelivered {
		t.Fatalf("resumed session did not progress: %v kWh", next.State.KWhDelivered)
	}
	if next.State.BatterySoC <= stored.BatterySoC {
		t.Fatalf("resumed session did not charge: %v %%", next.State.BatterySoC)
	}
}

func TestLocalGatewayFanoutToEverySubscriber(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	// Two clients watching the same charger (the app and the admin console) both
	// see every frame — the gateway fans out, it does not queue per reader.
	a, b := g.Subscribe(), g.Subscribe()
	defer g.Unsubscribe(a)
	defer g.Unsubscribe(b)

	const id = "ses_b7_fanout"
	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-06", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	for name, ch := range map[string]<-chan Telemetry{"subscriber A": a, "subscriber B": b} {
		if got := awaitTelemetry(t, ch, func(e Telemetry) bool { return e.SessionID == id }); got.SessionID != id {
			t.Errorf("%s received %q", name, got.SessionID)
		}
	}
}

func TestLocalGatewayUnsubscribeStopsDelivery(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	sub := g.Subscribe()
	g.Unsubscribe(sub)

	// A dropped subscription is simply never written to again, so this read must
	// time out — that is the assertion.
	if err := g.StartSession(ctx, StartCmd{
		SessionID: "ses_b7_gone", EVSEUID: "VLT-SGB-07", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	select {
	case e := <-sub:
		t.Fatalf("unsubscribed client still received %+v", e)
	case <-time.After(50 * time.Millisecond):
	}

	// A late SSE cleanup may call Unsubscribe twice, so the second call is safe.
	g.Unsubscribe(sub)
}

func TestLocalGatewayUnlockConnector(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	// Unlocking is a no-op locally — connector status lives in Postgres — but the
	// seam still has to reject a call with nothing to unlock.
	if err := g.UnlockConnector(ctx, "VLT-SGB-01"); err != nil {
		t.Fatalf("UnlockConnector: %v", err)
	}
	if err := g.UnlockConnector(ctx, ""); !IsCode(err, CodeValidation) {
		t.Fatalf("UnlockConnector with no EVSE = %v, want %s", err, CodeValidation)
	}
}

func TestLocalGatewayCloseEndsSessionsWithoutTerminalEvents(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	const id = "ses_b7_close"
	sub := g.Subscribe()

	if err := g.StartSession(ctx, StartCmd{
		SessionID: id, EVSEUID: "VLT-SGB-08", Params: dcParams(80), StartSoC: 20,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitTelemetry(t, sub, func(e Telemetry) bool { return e.SessionID == id })

	g.Close()

	// Shutdown invents nothing: the driver never stopped this session, so there
	// is no terminal event, just silence and a forgotten session.
	select {
	case e := <-sub:
		if e.SessionID == id && e.Terminal() {
			t.Fatalf("Close published a terminal event: %+v", e)
		}
	case <-time.After(50 * time.Millisecond):
	}

	if err := g.StopSession(ctx, id); !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("StopSession after Close = %v, want %s", err, CodeSessionNotFound)
	}
	if err := g.StartSession(ctx, StartCmd{
		SessionID: "ses_b7_after", EVSEUID: "VLT-SGB-08", Params: dcParams(80), StartSoC: 20,
	}); !IsCode(err, CodeCPOUnreachable) {
		t.Fatalf("StartSession after Close = %v, want %s", err, CodeCPOUnreachable)
	}

	// A subscriber that arrives after shutdown gets a closed channel instead of
	// one that will never carry anything.
	late := g.Subscribe()
	if _, ok := <-late; ok {
		t.Fatal("Subscribe after Close returned an open channel")
	}

	g.Close() // idempotent
}

func TestLocalGatewayDefaultsTheTimeScale(t *testing.T) {
	// SIM_TIME_SCALE is a demo knob, and a scale that makes a session outlive the
	// demo is not a scale at all, so anything below 1 falls back to the default.
	for _, scale := range []int{0, -5} {
		if got := NewLocalGateway(scale); got.scale != DefaultTimeScale {
			t.Errorf("NewLocalGateway(%d).scale = %d, want %d", scale, got.scale, DefaultTimeScale)
		}
	}
	if got := NewLocalGateway(30); got.scale != 30 {
		t.Errorf("NewLocalGateway(30).scale = %d, want 30", got.scale)
	}
}

func TestLocalGatewayRunsSessionsIndependently(t *testing.T) {
	g := fastGateway(t)
	ctx := context.Background()

	sub := g.Subscribe()
	defer g.Unsubscribe(sub)

	// Four chargers charging at once, each on its own goroutine. The simulator lets a
	// user hold several sessions, so one ending must not disturb the others.
	ids := []string{"ses_b7_par_1", "ses_b7_par_2", "ses_b7_par_3", "ses_b7_par_4"}
	errs := make(chan error, len(ids))
	for _, id := range ids {
		go func(id string) {
			errs <- g.StartSession(ctx, StartCmd{
				SessionID: id, EVSEUID: "VLT-SGB-01", Params: dcParams(20), StartSoC: 20,
			})
		}(id)
	}
	for range ids {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent StartSession: %v", err)
		}
	}

	// Each reaches its own target and reports it, in whatever order it finishes.
	finished := make(map[string]bool, len(ids))
	for len(finished) < len(ids) {
		e := awaitTelemetry(t, sub, func(e Telemetry) bool { return e.Terminal() })
		if e.Status != StatusCompleted || e.Reason != simulation.ReasonTargetReached {
			t.Fatalf("session %s ended %s / %s", e.SessionID, e.Status, e.Reason)
		}
		if e.State.BatterySoC != 20 {
			t.Fatalf("session %s finished at %v %%, want its 20 %% target", e.SessionID, e.State.BatterySoC)
		}
		finished[e.SessionID] = true
	}
	for _, id := range ids {
		if !finished[id] {
			t.Errorf("session %s never reported a terminal event", id)
		}
	}
}

func TestTelemetryTerminal(t *testing.T) {
	cases := map[string]bool{
		StatusCharging:  false,
		StatusCompleted: true,
		StatusFailed:    true,
		"":              false,
	}
	for status, want := range cases {
		if got := (Telemetry{Status: status}).Terminal(); got != want {
			t.Errorf("Terminal() for status %q = %v, want %v", status, got, want)
		}
	}
}

func TestGatewayErrorsCarryTheirAPICode(t *testing.T) {
	err := Errorf(CodeSessionNotFound, "session %s is not running", "ses_x")
	if got, want := err.Error(), "SESSION_NOT_FOUND: session ses_x is not running"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if CodeOf(err) != CodeSessionNotFound || !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("CodeOf(gateway error) = %q, want %s", CodeOf(err), CodeSessionNotFound)
	}
	if IsCode(err, CodeValidation) {
		t.Fatal("a SESSION_NOT_FOUND error matched VALIDATION_ERROR")
	}

	// the session manager wraps gateway errors on their way out of the orchestrator, so the code
	// has to survive wrapping — that is what errors.As in CodeOf buys.
	if code := CodeOf(fmt.Errorf("start session: %w", err)); code != CodeSessionNotFound {
		t.Fatalf("CodeOf(wrapped) = %q, want %s", code, CodeSessionNotFound)
	}

	// Plain errors have no gateway code and map to INTERNAL at the API boundary.
	if CodeOf(nil) != "" || IsCode(nil, CodeSessionNotFound) {
		t.Fatal("a nil error was reported as carrying a code")
	}
	if code := CodeOf(fmt.Errorf("plain failure")); code != "" {
		t.Fatalf("CodeOf(plain error) = %q, want \"\"", code)
	}
}

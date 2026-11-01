package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// uncertainStopGateway controls remote stop responses and terminal telemetry;
// the tests use the real isolated PostgreSQL store through newHarness.
type uncertainStopGateway struct {
	stopErr error
	onStop  func(context.Context, string) error
	events  chan gateway.Telemetry
}

func (g *uncertainStopGateway) StartSession(context.Context, gateway.StartCmd) error { return nil }
func (g *uncertainStopGateway) StopSession(ctx context.Context, id string) error {
	if g.onStop != nil {
		return g.onStop(ctx, id)
	}
	return g.stopErr
}
func (g *uncertainStopGateway) UnlockConnector(context.Context, string) error { return nil }
func (g *uncertainStopGateway) Subscribe() <-chan gateway.Telemetry           { return g.events }

func uncertainStopManager(t *testing.T, h *harness, g gateway.CPOGateway) *sessions.Manager {
	t.Helper()
	m := sessions.NewManager(h.st, g, h.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	m.Run(ctx)
	t.Cleanup(func() { cancel(); m.Shutdown() })
	return m
}

func TestEvidenceUnknownStopPreservesUncertainty(t *testing.T) {
	h := newHarness(t)
	g := &uncertainStopGateway{stopErr: gateway.Errorf(gateway.CodeSessionNotFound, "remote no longer identifies session; no terminal evidence"), events: make(chan gateway.Telemetry, 8)}
	m := uncertainStopManager(t, h, g)
	u := h.driver(100)
	s := h.start(m, u, 100)
	sub, _, err := m.Subscribe(context.Background(), u, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for attempt := 1; attempt <= 3; attempt++ {
		_, stopErr := m.Stop(context.Background(), u, s.ID)
		wantCode(t, stopErr, sessions.CodeCPOUnreachable)
		row, err := h.st.SessionByID(context.Background(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		w, err := h.st.GetWallet(context.Background(), u)
		if err != nil {
			t.Fatal(err)
		}
		var cdrs int
		if err := h.st.DB.QueryRow(`SELECT count(*) FROM cdrs WHERE session_id=$1`, s.ID).Scan(&cdrs); err != nil {
			t.Fatal(err)
		}
		n, amount := h.debitTotals(u)
		active, err := m.Active(context.Background(), u)
		if err != nil || active.ID != s.ID {
			t.Fatalf("uncertain stop lost active session: active=%+v err=%v", active, err)
		}
		t.Logf("phase=uncertain attempt=%d session=%s status=%s cdrs=%d debits=%d debit_amount=%.2f balance=%.2f reserved=%.2f connector=%s stop_error=%v", attempt, row.ID, row.Status, cdrs, n, amount, w.Balance, w.Reserved, h.connectorStatus(testEVSE), stopErr)
		if row.Terminal() || row.CDRID != nil || row.SettlementRequestedStatus != nil || cdrs != 0 || n != 0 || amount != 0 || w.Balance != 100 || w.Reserved != s.ReservedAmount || w.Reserved <= 0 || h.connectorStatus(testEVSE) != store.StatusCharging {
			t.Fatal("uncertain stop changed session/accounting or released the connector")
		}
		select {
		case frame, open := <-sub.C:
			if !open || frame.Terminal || frame.Session.Terminal() {
				t.Fatal("uncertain stop completed the live stream")
			}
		default:
		}
	}

	// Only a later authoritative event may settle the positive-energy charge.
	terminal := gateway.Telemetry{SessionID: s.ID, Status: gateway.StatusCompleted, Reason: simulation.ReasonUser,
		State: simulation.State{BatterySoC: s.BatterySoC + 1, KWhDelivered: 1, DurationSeconds: 60, Cost: s.TariffPerKWh}}
	g.events <- terminal
	done := h.waitFor(s.ID, settled)
	select {
	case frame, open := <-sub.C:
		if !open || !frame.Terminal || frame.Session.ID != s.ID {
			t.Fatal("authoritative settlement did not publish terminal frame")
		}
	case <-time.After(waitTimeout):
		t.Fatal("missing terminal stream event")
	}
	// Observe a duplicate terminal event being consumed before checking the
	// database, without a scheduling-dependent sleep.
	observer, _, err := m.Subscribe(context.Background(), u, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	g.events <- terminal
	select {
	case frame := <-observer.C:
		if !frame.Terminal {
			t.Fatal("duplicate terminal event did not complete its observer")
		}
	case <-time.After(waitTimeout):
		t.Fatal("duplicate terminal event was not consumed")
	}
	for range 3 {
		again, err := m.Stop(context.Background(), u, s.ID)
		if err != nil || again.ID != done.ID || again.CDRID == nil || *again.CDRID != *done.CDRID {
			t.Fatalf("settled retry changed result: row=%+v err=%v", again, err)
		}
	}
	w, err := h.st.GetWallet(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	n, amount := h.debitTotals(u)
	var cdrs int
	if err := h.st.DB.QueryRow(`SELECT count(*) FROM cdrs WHERE session_id=$1`, s.ID).Scan(&cdrs); err != nil {
		t.Fatal(err)
	}
	if cdrs != 1 || n != 1 || amount != done.TotalCost || w.Balance != 100-done.TotalCost || w.Reserved != 0 || done.KWhDelivered != 1 || done.TotalCost <= 0 || h.connectorStatus(testEVSE) != store.StatusAvailable {
		t.Fatalf("authoritative settlement mismatch: cdrs=%d debits=%d amount=%.2f wallet=%+v session=%+v", cdrs, n, amount, w, done)
	}
	t.Logf("phase=confirmed session=%s status=%s cdr=%s cdrs=%d debits=%d debit_amount=%.2f energy_kwh=%.3f balance=%.2f reserved=%.2f connector=AVAILABLE", done.ID, done.Status, *done.CDRID, cdrs, n, amount, done.KWhDelivered, w.Balance, w.Reserved)
}

func TestEvidenceUnknownStopAfterConfirmedSettlement(t *testing.T) {
	h := newHarness(t)
	g := &uncertainStopGateway{events: make(chan gateway.Telemetry, 8)}
	m := uncertainStopManager(t, h, g)
	u := h.driver(100)
	s := h.start(m, u, 100)
	// The terminal event arrives while the gateway is handling the command,
	// before it returns SESSION_NOT_FOUND. The durable result wins.
	g.onStop = func(_ context.Context, id string) error {
		g.events <- gateway.Telemetry{SessionID: id, Status: gateway.StatusCompleted, Reason: simulation.ReasonUser,
			State: simulation.State{BatterySoC: s.BatterySoC, KWhDelivered: 1, DurationSeconds: 60, Cost: s.TariffPerKWh}}
		h.waitFor(id, settled)
		return gateway.Errorf(gateway.CodeSessionNotFound, "terminal update arrived during stop")
	}
	done, err := m.Stop(context.Background(), u, s.ID)
	if err != nil || done == nil || !done.Terminal() || done.CDRID == nil {
		t.Fatalf("authoritative in-flight completion was lost: row=%+v err=%v", done, err)
	}
	n, amount := h.debitTotals(u)
	w, err := h.st.GetWallet(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	var cdrs int
	if err := h.st.DB.QueryRow(`SELECT count(*) FROM cdrs WHERE session_id=$1`, s.ID).Scan(&cdrs); err != nil {
		t.Fatal(err)
	}
	if n != 1 || amount != done.TotalCost || cdrs != 1 || w.Reserved != 0 {
		t.Fatalf("in-flight settlement mismatch: cdrs=%d debits=%d amount=%.2f wallet=%+v", cdrs, n, amount, w)
	}
	t.Logf("phase=confirmed_during_command session=%s cdr=%s cdrs=%d debits=%d balance=%.2f reserved=%.2f stop_error=nil", done.ID, *done.CDRID, cdrs, n, w.Balance, w.Reserved)
}

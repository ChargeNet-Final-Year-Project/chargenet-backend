// Package sessions_test drives the orchestrator against a real PostgreSQL and a
// real local simulator: the money path, the two auto-stop guards,
// the ordered start validation, the SSE fan-out and the restart recovery.
//
// Only the clock is faked: the gateway ticks every 50 ms and one tick is 150
// simulated seconds, so a 25→80 % DC charge settles in a couple of real seconds.
//
// The suite skips itself when no test database is reachable (testutil).
package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

const (
	// testEVSE is a seeded VoltGo CCS2 DC bay (station VLT-SGB-01): 350 kW
	// rated, 1.80 MYR/kWh, and a 150 kW acceptance figure for a driver with no
	// vehicle on file.
	testEVSE = "VLT-SGB-01"
	// testTick / testScale keep a demo charge a couple of real seconds long:
	// every 50 ms the simulator advances 150 simulated seconds.
	testTick  = 50 * time.Millisecond
	testScale = 150
	// waitTimeout bounds every "wait until the session reaches X" poll.
	waitTimeout = 20 * time.Second
)

// harness wires the real store, the local simulator and the orchestrator the way
// cmd/server does, but with a fast clock and a shared seed connector.
type harness struct {
	t   *testing.T
	st  *store.Store
	gw  *gateway.LocalGateway
	cfg config.Config
	seq atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:  t,
		st: testutil.NewTestStore(t),
		cfg: config.Config{
			// JWTSecret is unused at this layer; the config only carries the
			// money knobs the orchestrator reads.
			JWTSecret:       "test-secret",
			MinStartBalance: 10,
			PlatformFeePct:  5,
		},
	}
	h.gw = h.newGateway()
	t.Cleanup(func() { h.releaseEVSE(false) })
	h.releaseEVSE(true)
	return h
}

// newGateway builds a simulator on the harness clock. The restart test needs a
// second one to stand in for a fresh process.
func (h *harness) newGateway() *gateway.LocalGateway {
	gw := gateway.NewLocalGateway(testScale, gateway.WithTick(testTick))
	h.t.Cleanup(gw.Close)
	return gw
}

// releaseEVSE puts the shared seed connector back to AVAILABLE so an interrupted
// run cannot strand the bay as CHARGING for the next test. The fatal variant is
// for the pre-test reset: in a cleanup function only Errorf is allowed.
func (h *harness) releaseEVSE(fatal bool) {
	fail := h.t.Errorf
	if fatal {
		fail = h.t.Fatalf
	}
	_, conn, err := h.st.GetEVSE(context.Background(), testEVSE)
	if err != nil {
		fail("releaseEVSE: GetEVSE %s: %v", testEVSE, err)
		return
	}
	if err := h.st.SetConnectorStatus(context.Background(), conn.ID, store.StatusAvailable); err != nil {
		fail("releaseEVSE: SetConnectorStatus: %v", err)
	}
}

// run builds a manager on this harness's seams and starts it the way main.go
// does: Run performs the recovery pass inline and only then leaves the consumer
// running in the background. Calling it on this goroutine (never in a goroutine
// of its own) is what keeps the recovery scan from racing the test's first
// Start — a race that would make recovery mistake the brand-new row for a
// leftover one and fail it. The returned stop function is idempotent and also
// runs at cleanup, so a test that simulates a restart can play the process death
// explicitly first.
func (h *harness) run(opts ...sessions.Option) (*sessions.Manager, func()) {
	h.t.Helper()
	opts = append([]sessions.Option{sessions.WithTelemetryInterval(10 * time.Millisecond)}, opts...)
	mgr := sessions.NewManager(h.st, h.gw, h.cfg, opts...)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Run(ctx)

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()       // the consumer stops reading gateway events
			mgr.Shutdown() // open SSE clients finish their responses
		})
	}
	h.t.Cleanup(stop)
	return mgr, stop
}

// driver registers a driver with the given starting balance and returns its id.
func (h *harness) driver(balance float64) string {
	h.t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("driver%d@example.com", h.seq.Add(1))
	user, err := h.st.CreateUser(ctx, "Test Driver", email, "hash")
	if err != nil {
		h.t.Fatalf("CreateUser: %v", err)
	}
	if balance > 0 {
		if _, err := h.st.TopUpWallet(ctx, user.ID, balance, store.PaymentMaybankFPX); err != nil {
			h.t.Fatalf("TopUpWallet: %v", err)
		}
	}
	return user.ID
}

// start opens a charge on the shared bay, the way POST /sessions/start does.
func (h *harness) start(mgr *sessions.Manager, userID string, targetSoC float64) *store.Session {
	h.t.Helper()
	quote, err := h.st.CreateQuote(context.Background(), userID, testEVSE, "15")
	if err != nil {
		h.t.Fatalf("quote: %v", err)
	}
	sess, err := mgr.Start(context.Background(), userID, sessions.StartRequest{
		QuoteID:   quote.ID,
		EVSEUID:   testEVSE,
		TargetSoC: targetSoC,
	})
	if err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	return sess
}

// waitFor polls the row until want holds, or fails after waitTimeout with the
// last row it saw so the failure is readable.
func (h *harness) waitFor(sessionID string, want func(store.Session) bool) store.Session {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last store.Session
	for time.Now().Before(deadline) {
		row, err := h.st.SessionByID(context.Background(), sessionID)
		if err != nil {
			h.t.Fatalf("SessionByID(%s): %v", sessionID, err)
		}
		last = *row
		if want(*row) {
			return *row
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("session %s never reached the expected state (last: status=%s kwh=%.3f soc=%.2f cost=%.2f)",
		sessionID, last.Status, last.KWhDelivered, last.BatterySoC, last.TotalCost)
	return last
}

// settled is the usual waitFor predicate: the row is terminal and its CDR (and
// therefore its money) has been written.
func settled(s store.Session) bool { return s.Terminal() && s.CDRID != nil }

// connectorStatus reads a bay's current status.
func (h *harness) connectorStatus(evseUID string) string {
	h.t.Helper()
	_, conn, err := h.st.GetEVSE(context.Background(), evseUID)
	if err != nil {
		h.t.Fatalf("GetEVSE(%s): %v", evseUID, err)
	}
	return conn.Status
}

// debitTotals counts and sums the CHARGING_DEBIT rows in a driver's ledger.
func (h *harness) debitTotals(userID string) (int, float64) {
	h.t.Helper()
	var n int
	var sum float64
	err := h.st.DB.QueryRowContext(context.Background(),
		`SELECT count(*), coalesce(sum(t.amount), 0)::float8
		   FROM wallet_transactions t JOIN wallets w ON w.id = t.wallet_id
		  WHERE w.user_id = $1 AND t.type = $2`,
		userID, store.TxTypeChargingDebit).Scan(&n, &sum)
	if err != nil {
		h.t.Fatalf("debit totals: %v", err)
	}
	return n, sum
}

// cdr is the persisted OCPI ChargeDetailRecord of a session, as the CPO
// dashboard and the payout report read it.
type cdr struct {
	ID          string
	EnergyKWh   float64
	DurationSec int
	Gross       float64
	Fee         float64
	Net         float64
	Currency    string
	PartyID     string
	TariffID    string
	PricePerKWh float64
}

// loadCDR reads the settled CDR of a session from Postgres.
func (h *harness) loadCDR(sessionID string) cdr {
	h.t.Helper()
	var c cdr
	err := h.st.DB.QueryRowContext(context.Background(),
		`SELECT id, total_energy_kwh::float8, total_time_seconds,
		        gross_amount::float8, platform_fee::float8, net_payout::float8, currency,
		        cdr_json->>'party_id', cdr_json->'tariffs'->0->>'id',
		        (cdr_json->'tariffs'->0->'elements'->0->'price_components'->0->>'price')::float8
		   FROM cdrs WHERE session_id = $1`, sessionID).
		Scan(&c.ID, &c.EnergyKWh, &c.DurationSec, &c.Gross, &c.Fee, &c.Net, &c.Currency,
			&c.PartyID, &c.TariffID, &c.PricePerKWh)
	if err != nil {
		h.t.Fatalf("load CDR of %s: %v", sessionID, err)
	}
	return c
}

// wantCode asserts err carries the given orchestrator code and returns it.
func wantCode(t *testing.T, err error, code string) *sessions.Error {
	t.Helper()
	var se *sessions.Error
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a sessions.Error carrying %s", err, code)
	}
	if se.Code != code {
		t.Fatalf("error code = %s, want %s (%v)", se.Code, code, err)
	}
	return se
}

// str dereferences an optional string assertively.
func str(t *testing.T, p *string, what string) string {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil", what)
	}
	return *p
}

// close3 compares energy figures, which both sides round to 3 decimals.
func close3(a, b float64) bool { return math.Abs(a-b) < 0.001 }

// TestStartChargingSettlesAtTarget is the session manager acceptance walk: a funded driver
// starts on the seed bay, telemetry lands, the target SoC ends the charge, and
// the session row, the CDR, the wallet ledger and the connector all agree.
func TestStartChargingSettlesAtTarget(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	userID := h.driver(100)

	sess := h.start(mgr, userID, 80)
	if sess.Status != store.StatusCharging {
		t.Fatalf("status right after start = %s, want CHARGING", sess.Status)
	}
	if sess.EVSEUID != testEVSE || sess.TariffPerKWh != 1.80 || sess.TargetSoC != 80 {
		t.Fatalf("start row = %+v", sess)
	}
	// The joined names are what the monitor screen renders above the numbers.
	if sess.CpoName != "VoltGo" || sess.StationName == "" || sess.ConnectorID == "" {
		t.Fatalf("start row is missing joined names: %+v", sess)
	}
	if got := h.connectorStatus(testEVSE); got != store.StatusCharging {
		t.Fatalf("connector status during the charge = %s, want CHARGING", got)
	}

	done := h.waitFor(sess.ID, settled)
	if done.Status != store.StatusCompleted {
		t.Fatalf("final status = %s (%s), want COMPLETED", done.Status, str(t, done.StopReason, "stopReason"))
	}
	if got := str(t, done.StopReason, "stopReason"); got != simulation.ReasonTargetReached {
		t.Fatalf("stopReason = %s, want %s", got, simulation.ReasonTargetReached)
	}
	if done.EndTime == nil || done.BatterySoC < 79.9 || done.BatterySoC > 80.1 {
		t.Fatalf("final row = %+v (endTime/soc), want an 80 %% target", done)
	}
	if done.KWhDelivered <= 0 || done.DurationSeconds <= 0 {
		t.Fatalf("final row = %+v, want energy and time delivered", done)
	}
	if len(done.CDRJSON) == 0 {
		t.Fatal("the session row carries no cdrJson after settlement")
	}

	// Money: the session row, the CDR and the ledger must be one number.
	cdr := h.loadCDR(sess.ID)
	wantCost := cdr.Gross
	if done.TotalCost != wantCost {
		t.Fatalf("session totalCost = %.2f, CDR gross = %.2f", done.TotalCost, wantCost)
	}
	if !close3(cdr.EnergyKWh, done.KWhDelivered) {
		t.Fatalf("CDR energy = %.3f, session energy = %.3f", cdr.EnergyKWh, done.KWhDelivered)
	}
	if want := store.Round2(done.KWhDelivered * done.TariffPerKWh); math.Abs(cdr.Gross-want) > 0.01 {
		t.Fatalf("gross = %.2f, want about %.2f (%.3f kWh x %.2f)",
			cdr.Gross, want, done.KWhDelivered, done.TariffPerKWh)
	}
	if want := store.Round2(cdr.Gross * 5 / 100); cdr.Fee != want {
		t.Fatalf("platform fee = %.2f, want %.2f (5 %% of %.2f)", cdr.Fee, want, cdr.Gross)
	}
	if want := store.Round2(cdr.Gross - cdr.Fee); cdr.Net != want {
		t.Fatalf("net payout = %.2f, want %.2f", cdr.Net, want)
	}
	if cdr.Currency != store.CurrencyMYR {
		t.Fatalf("CDR currency = %q, want %q", cdr.Currency, store.CurrencyMYR)
	}
	// The document is built from the CPO and the connector, not from the session
	// row, so these prove the join survived settlement.
	if cdr.PartyID != "VLT" || cdr.TariffID != "trf_voltgo_350" || cdr.PricePerKWh != 1.80 {
		t.Fatalf("CDR identity = %+v, want VLT / trf_voltgo_350 / 1.80", cdr)
	}

	if done.BalanceAfter == nil || *done.BalanceAfter != store.Round2(100-wantCost) {
		t.Fatalf("balanceAfter = %v, want %.2f", done.BalanceAfter, store.Round2(100-wantCost))
	}
	wallet, err := h.st.GetWallet(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if wallet.Balance != *done.BalanceAfter {
		t.Fatalf("wallet balance = %.2f, session balanceAfter = %.2f", wallet.Balance, *done.BalanceAfter)
	}
	if n, sum := h.debitTotals(userID); n != 1 || sum != wantCost {
		t.Fatalf("ledger = %d debits totalling %.2f, want one of %.2f", n, sum, wantCost)
	}

	// The debit references the CDR, which is what the payout report joins on.
	txs, err := h.st.ListWalletTransactions(context.Background(), userID, 10, 0)
	if err != nil {
		t.Fatalf("ListWalletTransactions: %v", err)
	}
	if len(txs) != 2 { // the TOPUP and the CHARGING_DEBIT
		t.Fatalf("ledger entries = %d, want 2", len(txs))
	}
	if tx := txs[0]; tx.Type != store.TxTypeChargingDebit || tx.Amount != wantCost ||
		tx.ReferenceID != cdr.ID || tx.PaymentMethod != store.PaymentWallet {
		t.Fatalf("debit entry = %+v, want %.2f MYR referencing %s", tx, wantCost, cdr.ID)
	}

	// The bay is free again and the charge is the driver's newest history row.
	if got := h.connectorStatus(testEVSE); got != store.StatusAvailable {
		t.Fatalf("connector status after settlement = %s, want AVAILABLE", got)
	}
	history, err := mgr.History(context.Background(), userID, 10, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 || history[0].ID != sess.ID {
		t.Fatalf("history = %+v, want the settled session", history)
	}
	if _, err := mgr.Active(context.Background(), userID); !sessions.CodeIs(err, sessions.CodeNoActiveSession) {
		t.Fatalf("Active after a finished charge = %v, want NO_ACTIVE_SESSION", err)
	}
}

// TestWalletExhaustedStopsAndClampsSettlement covers the guard only the
// orchestrator can evaluate: a driver whose balance runs out mid
// charge is stopped with BALANCE_EXHAUSTED and billed exactly what is left.
func TestWalletExhaustedStopsAndClampsSettlement(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)

	const start = 10.00 // exactly the wallet floor, and far below the 80 % cost
	userID := h.driver(start)

	// Target 100 %: the charge cannot end on its own, so the wallet guard is the
	// only thing that can stop it.
	sess := h.start(mgr, userID, 100)
	done := h.waitFor(sess.ID, settled)

	if done.Status != store.StatusCompleted {
		t.Fatalf("final status = %s, want COMPLETED", done.Status)
	}
	if got := str(t, done.StopReason, "stopReason"); got != simulation.ReasonBalanceExhausted {
		t.Fatalf("stopReason = %s, want %s", got, simulation.ReasonBalanceExhausted)
	}
	if done.TotalCost > start || done.TotalCost < start-0.02 {
		t.Fatalf("totalCost = %.2f, want the balance (%.2f) and no more", done.TotalCost, start)
	}
	if done.BalanceAfter == nil || *done.BalanceAfter > 0.02 {
		t.Fatalf("balanceAfter = %v, want the wallet drained", done.BalanceAfter)
	}
	if *done.BalanceAfter != store.Round2(start-done.TotalCost) {
		t.Fatalf("balanceAfter = %.2f, want %.2f", *done.BalanceAfter, store.Round2(start-done.TotalCost))
	}
	if cdr := h.loadCDR(sess.ID); cdr.Gross != done.OriginalCost || math.Abs(cdr.Gross-done.TotalCost-done.UnfundedAmount) > 0.001 {
		t.Fatalf("operator gross %.2f, wallet deduction %.2f, liability %.2f", cdr.Gross, done.TotalCost, done.UnfundedAmount)
	}
	if n, sum := h.debitTotals(userID); n != 1 || sum != done.TotalCost {
		t.Fatalf("ledger = %d debits totalling %.2f, want one of %.2f", n, sum, done.TotalCost)
	}
	if got := h.connectorStatus(testEVSE); got != store.StatusAvailable {
		t.Fatalf("connector status after the wallet stop = %s, want AVAILABLE", got)
	}
}

// TestStopSettlesExactlyOnce is the idempotency requirement : a
// driver's stop, a double tap and a retry all settle the same single debit.
func TestStopSettlesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	userID := h.driver(100)

	// Target 100 % so the charge is still running when the driver stops it.
	sess := h.start(mgr, userID, 100)
	h.waitFor(sess.ID, func(s store.Session) bool {
		return s.Status == store.StatusCharging && s.KWhDelivered > 0
	})

	first, err := mgr.Stop(context.Background(), userID, sess.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !first.Terminal() {
		t.Fatalf("Stop returned %s, want a terminal row", first.Status)
	}
	if got := str(t, first.StopReason, "stopReason"); got != simulation.ReasonUser {
		t.Fatalf("stopReason = %s, want %s", got, simulation.ReasonUser)
	}
	if first.CDRID == nil || first.KWhDelivered <= 0 {
		t.Fatalf("stopped session = %+v, want a CDR and some energy", first)
	}

	// The retry: same row, no second settlement.
	second, err := mgr.Stop(context.Background(), userID, sess.ID)
	if err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if second.CDRID == nil || *second.CDRID != *first.CDRID {
		t.Fatalf("retry CDR = %v, want the same %s", second.CDRID, *first.CDRID)
	}
	if second.TotalCost != first.TotalCost || *second.BalanceAfter != *first.BalanceAfter {
		t.Fatalf("retry money = %+v, want %+v", second, first)
	}

	var cdrRows int
	if err := h.st.DB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM cdrs WHERE session_id = $1`, sess.ID).Scan(&cdrRows); err != nil {
		t.Fatalf("count CDRs: %v", err)
	}
	if cdrRows != 1 {
		t.Fatalf("CDR rows = %d, want 1", cdrRows)
	}
	if n, sum := h.debitTotals(userID); n != 1 || sum != first.TotalCost {
		t.Fatalf("ledger = %d debits totalling %.2f, want one of %.2f", n, sum, first.TotalCost)
	}
	if got := h.connectorStatus(testEVSE); got != store.StatusAvailable {
		t.Fatalf("connector status after the stop = %s, want AVAILABLE", got)
	}
}

// TestStreamCarriesTelemetryThenCompleted exercises the fan-out the mobile
// monitor screen subscribes to: live frames while the charge runs, then exactly
// one terminal frame whose payload is the settled row (CDR included).
func TestStreamCarriesTelemetryThenCompleted(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	userID := h.driver(100)

	sess := h.start(mgr, userID, 80)

	sub, seed, err := mgr.Subscribe(context.Background(), userID, sess.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	if seed.ID != sess.ID {
		t.Fatalf("stream seed = %s, want %s", seed.ID, sess.ID)
	}

	telemetry, terminal := 0, 0
	var last store.Session
	deadline := time.After(waitTimeout)

loop:
	for {
		select {
		case frame, open := <-sub.C:
			if !open {
				break loop // the hub closes the channel right after the terminal frame
			}
			last = frame.Session
			if frame.Terminal {
				terminal++
				break loop
			}
			telemetry++
			if frame.Session.Status != store.StatusCharging {
				t.Fatalf("live frame status = %s, want CHARGING", frame.Session.Status)
			}
		case <-deadline:
			t.Fatalf("stream went quiet after %d telemetry frames", telemetry)
		}
	}

	if telemetry < 2 {
		t.Fatalf("telemetry frames = %d, want at least 2", telemetry)
	}
	if terminal != 1 {
		t.Fatalf("terminal frames = %d, want exactly 1", terminal)
	}
	if last.Status != store.StatusCompleted || last.CDRID == nil || last.EndTime == nil {
		t.Fatalf("terminal frame = %+v, want the settled row", last)
	}
	if last.TotalCost != h.loadCDR(sess.ID).Gross {
		t.Fatalf("terminal frame cost = %.2f, CDR gross = %.2f", last.TotalCost, h.loadCDR(sess.ID).Gross)
	}
	// A subscriber that connects after the charge ended is seeded straight with
	// the terminal row (the handler turns that into `event: completed`).
	late, row, err := mgr.Subscribe(context.Background(), userID, sess.ID)
	if err != nil {
		t.Fatalf("late Subscribe: %v", err)
	}
	late.Close()
	if !row.Terminal() || row.CDRID == nil {
		t.Fatalf("late seed = %+v, want the settled row", row)
	}
}

// TestSubscribeHidesAnotherDriversSession is the ownership rule the SSE route
// relies on: someone else's session id is indistinguishable from an unknown one.
func TestSubscribeHidesAnotherDriversSession(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	owner := h.driver(100)
	intruder := h.driver(100)

	sess := h.start(mgr, owner, 100)

	if _, _, err := mgr.Subscribe(context.Background(), intruder, sess.ID); !sessions.CodeIs(err, sessions.CodeSessionNotFound) {
		t.Fatalf("intruder Subscribe = %v, want SESSION_NOT_FOUND", err)
	}
	if _, err := mgr.Get(context.Background(), intruder, sess.ID); !sessions.CodeIs(err, sessions.CodeSessionNotFound) {
		t.Fatalf("intruder Get = %v, want SESSION_NOT_FOUND", err)
	}
	if _, err := mgr.Stop(context.Background(), intruder, sess.ID); !sessions.CodeIs(err, sessions.CodeSessionNotFound) {
		t.Fatalf("intruder Stop = %v, want SESSION_NOT_FOUND", err)
	}
	// And the refused stop really did nothing.
	row, err := mgr.Get(context.Background(), owner, sess.ID)
	if err != nil {
		t.Fatalf("owner Get: %v", err)
	}
	if row.Status != store.StatusCharging {
		t.Fatalf("session status after the refused stop = %s, want CHARGING", row.Status)
	}
	if _, err := mgr.Active(context.Background(), intruder); !sessions.CodeIs(err, sessions.CodeNoActiveSession) {
		t.Fatalf("intruder Active = %v, want NO_ACTIVE_SESSION", err)
	}
}

// TestStartValidationOrder walks ordered validation through the
// orchestrator and asserts the code each failure carries. The order is the
// contract: a driver with two problems must see the more actionable one.
func TestStartValidationOrder(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	ctx := context.Background()
	clean := h.driver(100)

	// Step 1 — the request itself.
	if _, err := mgr.Start(ctx, clean, sessions.StartRequest{}); !sessions.CodeIs(err, sessions.CodeValidation) {
		t.Fatalf("missing evseUid = %v, want VALIDATION_ERROR", err)
	}
	if _, err := mgr.Start(ctx, clean, sessions.StartRequest{EVSEUID: testEVSE, TargetSoC: 150}); !sessions.CodeIs(err, sessions.CodeValidation) {
		t.Fatalf("targetSoc 150 = %v, want VALIDATION_ERROR", err)
	}

	// Step 2 — the wallet floor (this driver's balance is 0.00).
	poor := h.driver(0)
	_, poorErr := mgr.Start(ctx, poor, sessions.StartRequest{EVSEUID: testEVSE})
	se := wantCode(t, poorErr, sessions.CodeInsufficientBalance)
	if se.Required == nil || *se.Required != 10 || se.Balance == nil || *se.Balance != 0 {
		t.Fatalf("insufficient balance error = %+v, want required 10.00 / balance 0.00", se)
	}

	// Step 3 — an unknown vehicle id.
	unknown := h.driver(100)
	if _, err := mgr.Start(ctx, unknown, sessions.StartRequest{EVSEUID: "NOPE-000-01"}); !sessions.CodeIs(err, sessions.CodeEVSENotFound) {
		t.Fatalf("unknown EVSE = %v, want EVSE_NOT_FOUND", err)
	}
	if _, err := mgr.Start(ctx, unknown, sessions.StartRequest{EVSEUID: testEVSE, VehicleID: "veh_does_not_exist"}); !sessions.CodeIs(err, sessions.CodeVehicleNotFound) {
		t.Fatalf("unknown vehicle = %v, want VEHICLE_NOT_FOUND", err)
	}

	// Step 4 — an incompatible vehicle on a CCS2 bay. A driver's first vehicle
	// becomes their default, so this also covers the implicit-vehicle path.
	car := h.driver(100)
	veh, err := h.st.CreateVehicle(ctx, car, store.VehicleInput{
		Name: "e-Golf", LicensePlate: "WXY1234", ConnectorType: store.StandardTYPE2, BatteryCapacity: 35,
	})
	if err != nil {
		t.Fatalf("CreateVehicle: %v", err)
	}
	if _, err := mgr.Start(ctx, car, sessions.StartRequest{EVSEUID: testEVSE, VehicleID: veh.ID}); !sessions.CodeIs(err, sessions.CodeIncompatibleConnector) {
		t.Fatalf("TYPE2 car on a CCS2 bay = %v, want INCOMPATIBLE_CONNECTOR", err)
	}
	if _, err := mgr.Start(ctx, car, sessions.StartRequest{EVSEUID: testEVSE}); !sessions.CodeIs(err, sessions.CodeIncompatibleConnector) {
		t.Fatalf("implicit default vehicle = %v, want INCOMPATIBLE_CONNECTOR", err)
	}

	// A rejected start must leave nothing behind.
	for userID, want := range map[string]int{poor: 0, unknown: 0, car: 0} {
		var rows int
		if err := h.st.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM charging_sessions WHERE user_id = $1`, userID).Scan(&rows); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if rows != want {
			t.Fatalf("driver %s has %d session rows after rejected starts, want %d", userID, rows, want)
		}
	}

	// Step 5 — a good start, then the duplicate and the busy bay.
	sess := h.start(mgr, clean, 80)
	if _, err := mgr.Start(ctx, clean, sessions.StartRequest{EVSEUID: testEVSE}); !sessions.CodeIs(err, sessions.CodeSessionAlreadyActive) {
		t.Fatalf("second session for one driver = %v, want SESSION_ALREADY_ACTIVE", err)
	}
	if _, err := mgr.Start(ctx, unknown, sessions.StartRequest{EVSEUID: testEVSE}); !sessions.CodeIs(err, sessions.CodeConnectorUnavailable) {
		t.Fatalf("second driver on a busy bay = %v, want CONNECTOR_UNAVAILABLE", err)
	}

	if _, err := mgr.Stop(ctx, clean, sess.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.connectorStatus(testEVSE); got != store.StatusAvailable {
		t.Fatalf("connector status after the stop = %s, want AVAILABLE", got)
	}
}

// suspendCpo suspends a partner for the duration of the test and puts the row
// back exactly as it was afterwards. The seed CPOs are shared with every other
// test in the package — testutil resets the user tables, not the network ones —
// so a partner left SUSPENDED would hide its stations from all of them.
func (h *harness) suspendCpo(cpoID string) {
	h.t.Helper()
	ctx := context.Background()

	var status, handshake string
	var tokA, tokB, tokC *string
	err := h.st.DB.QueryRowContext(ctx,
		`SELECT status, handshake_status, token_a, token_b, token_c
		   FROM cpo_partners WHERE id = $1`, cpoID).
		Scan(&status, &handshake, &tokA, &tokB, &tokC)
	if err != nil {
		h.t.Fatalf("snapshot partner %s: %v", cpoID, err)
	}
	h.t.Cleanup(func() {
		if _, err := h.st.DB.ExecContext(context.Background(),
			`UPDATE cpo_partners
			    SET status = $2, handshake_status = $3, token_a = $4, token_b = $5, token_c = $6
			  WHERE id = $1`, cpoID, status, handshake, tokA, tokB, tokC); err != nil {
			h.t.Errorf("restore partner %s: %v", cpoID, err)
		}
	})

	if err := h.st.SuspendCPO(ctx, cpoID); err != nil {
		h.t.Fatalf("SuspendCPO(%s): %v", cpoID, err)
	}
}

// TestStartRejectsSuspendedCpo covers a saved station link after suspension. The
// start returns CONNECTOR_UNAVAILABLE and creates no session row.
func TestStartRejectsSuspendedCpo(t *testing.T) {
	h := newHarness(t)
	mgr, _ := h.run()
	h.releaseEVSE(true)
	ctx := context.Background()

	// The seed bay's owner is whatever partner the seed data assigns it.
	station, _, err := h.st.GetEVSE(ctx, testEVSE)
	if err != nil {
		t.Fatalf("GetEVSE: %v", err)
	}
	h.suspendCpo(station.CpoID)

	userID := h.driver(100)
	if _, err := mgr.Start(ctx, userID, sessions.StartRequest{EVSEUID: testEVSE}); !sessions.CodeIs(err, sessions.CodeConnectorUnavailable) {
		t.Fatalf("start on a suspended partner's bay = %v, want CONNECTOR_UNAVAILABLE", err)
	}

	var rows int
	if err := h.st.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM charging_sessions WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a rejected start left %d session rows behind", rows)
	}
}

// TestRestartResumesInterruptedSession is the session manager recovery path: a process that
// dies mid-charge comes back, re-arms the CHARGING row from its stored state on
// a brand-new transport, and settles it once when it finishes.
func TestRestartResumesInterruptedSession(t *testing.T) {
	h := newHarness(t)
	mgr, stop := h.run()
	h.releaseEVSE(true)
	userID := h.driver(100)

	sess := h.start(mgr, userID, 80)
	before := h.waitFor(sess.ID, func(s store.Session) bool {
		return s.Status == store.StatusCharging && s.KWhDelivered > 0
	})

	// The restart: the consumer dies with the row still CHARGING and the
	// connector still held. The bay, the wallet and the row all survive.
	stop()
	h.gw = h.newGateway() // a fresh process gets a fresh transport
	mgr2, _ := h.run()

	done := h.waitFor(sess.ID, settled)
	if done.Status != store.StatusCompleted {
		t.Fatalf("resumed session status = %s, want COMPLETED", done.Status)
	}
	if done.KWhDelivered <= before.KWhDelivered || done.DurationSeconds <= before.DurationSeconds {
		t.Fatalf("resumed charge lost its progress: before kwh=%.3f t=%ds, after kwh=%.3f t=%ds",
			before.KWhDelivered, before.DurationSeconds, done.KWhDelivered, done.DurationSeconds)
	}
	if done.BatterySoC < before.BatterySoC {
		t.Fatalf("resumed SoC went backwards: %.2f -> %.2f", before.BatterySoC, done.BatterySoC)
	}
	// One settlement: the resumed charge bills the whole thing exactly once.
	if n, sum := h.debitTotals(userID); n != 1 || sum != done.TotalCost {
		t.Fatalf("ledger = %d debits totalling %.2f, want one of %.2f", n, sum, done.TotalCost)
	}
	if cdr := h.loadCDR(sess.ID); cdr.Gross != done.OriginalCost || math.Abs(cdr.Gross-done.TotalCost-done.UnfundedAmount) > 0.001 {
		t.Fatalf("operator gross %.2f, wallet deduction %.2f, liability %.2f", cdr.Gross, done.TotalCost, done.UnfundedAmount)
	}
	if got := h.connectorStatus(testEVSE); got != store.StatusAvailable {
		t.Fatalf("connector status after the resumed charge = %s, want AVAILABLE", got)
	}
	if _, err := mgr2.Active(context.Background(), userID); !sessions.CodeIs(err, sessions.CodeNoActiveSession) {
		t.Fatalf("Active after recovery = %v, want NO_ACTIVE_SESSION", err)
	}
}

func TestRestartFinishesDurableSettlementAndReleasesHold(t *testing.T) {
	h := newHarness(t)
	mgr, stop := h.run()
	user := h.driver(100)
	sess := h.start(mgr, user, 100)
	snapshot := h.waitFor(sess.ID, func(s store.Session) bool { return s.KWhDelivered > 0 })
	stop()
	if err := h.st.RequestSettlement(context.Background(), sess.ID, store.StatusCompleted, simulation.ReasonUser, store.SessionTelemetry{
		KWhDelivered: snapshot.KWhDelivered, DurationSeconds: snapshot.DurationSeconds, BatterySoC: snapshot.BatterySoC,
	}); err != nil {
		t.Fatal(err)
	}
	h.gw = h.newGateway()
	h.run()
	done := h.waitFor(sess.ID, settled)
	if done.KWhDelivered != snapshot.KWhDelivered {
		t.Fatal("recovery restarted an already stopped charger")
	}
	if done.QuoteID == nil || done.ExchangeRate != sess.ExchangeRate {
		t.Fatal("recovery lost accepted pricing")
	}
	wallet, err := h.st.GetWallet(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	if wallet.Reserved != 0 || wallet.Balance != 100-done.TotalCost {
		t.Fatalf("reconciled wallet %+v", wallet)
	}
	if n, _ := h.debitTotals(user); n != 1 {
		t.Fatalf("recovery settled %d times", n)
	}
}

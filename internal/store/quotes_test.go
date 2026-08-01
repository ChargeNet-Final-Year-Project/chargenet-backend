package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

func quoteDriver(t *testing.T, st *store.Store, currency string) string {
	t.Helper()
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "FX driver", store.NewID("fx")+"@example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.ExecContext(ctx, `UPDATE wallets SET currency=$2,balance=1000 WHERE user_id=$1`, u.ID, currency); err != nil {
		t.Fatal(err)
	}
	return u.ID
}
func quoteEVSE(t *testing.T, st *store.Store, country string) string {
	t.Helper()
	var uid string
	if err := st.DB.QueryRow(`SELECT k.evse_uid FROM connectors k JOIN stations s ON s.id=k.station_id WHERE s.country_code=$1 AND s.is_simulated AND k.power_type='DC' AND k.status='AVAILABLE' ORDER BY s.id LIMIT 1`, country).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	return uid
}
func accept(t *testing.T, st *store.Store, user, uid string, q *store.ChargingQuote) (*store.Session, error) {
	t.Helper()
	station, conn, err := st.GetEVSE(context.Background(), uid)
	if err != nil {
		return nil, err
	}
	return st.InsertPendingSession(context.Background(), store.PendingSessionInput{UserID: user, QuoteID: q.ID, CPOID: station.CpoID, StationID: station.ID, ConnectorID: conn.ID, EVSEUID: uid, TargetSoC: 80, TariffPerKWh: conn.TariffPerKWh})
}
func TestQuoteCurrencyMatrix(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	for _, wallet := range []string{"MYR", "SGD", "THB"} {
		for _, country := range []string{"MY", "SG", "TH"} {
			t.Run(wallet+"_"+country, func(t *testing.T) {
				user := quoteDriver(t, st, wallet)
				uid := quoteEVSE(t, st, country)
				q, err := st.CreateQuote(ctx, user, uid, "15")
				if err != nil {
					t.Fatal(err)
				}
				if q.WalletCurrency != wallet {
					t.Fatalf("wallet currency %s", q.WalletCurrency)
				}
				if q.OriginalCurrency == wallet && q.ExchangeRate != "1.0000000000" {
					t.Fatalf("same currency rate %s", q.ExchangeRate)
				}
				session, err := accept(t, st, user, uid, q)
				if err != nil {
					t.Fatal(err)
				}
				w, err := st.GetWallet(ctx, user)
				if err != nil {
					t.Fatal(err)
				}
				if w.Reserved != 1000 || w.AvailableBalance != 0 {
					t.Fatalf("hold %+v", w)
				}
				tx, _ := st.DB.BeginTx(ctx, nil)
				_, err = store.DebitWallet(ctx, tx, user, 1, "other")
				tx.Rollback()
				if !errors.Is(err, store.ErrInsufficientBalance) {
					t.Fatalf("held funds spent: %v", err)
				}
				done, err := st.SettleSession(ctx, store.SettleInput{SessionID: session.ID, State: store.SessionTelemetry{KWhDelivered: 15, DurationSeconds: 600}, FallbackFeePct: 5, BuildCDR: sessions.BuildCDR})
				if err != nil {
					t.Fatal(err)
				}
				if done.OriginalCurrency != q.OriginalCurrency || done.WalletCurrency != wallet || done.OriginalCost <= 0 {
					t.Fatalf("snapshot %+v", done)
				}
				var matches bool
				if err = st.DB.QueryRow(`SELECT s.total_cost=q.estimated_wallet_cost AND s.original_cost=q.estimated_original_cost FROM charging_sessions s JOIN charging_quotes q ON q.id=s.quote_id WHERE s.id=$1`, session.ID).Scan(&matches); err != nil || !matches {
					t.Fatalf("quote/settlement mismatch %v", err)
				}
				w, _ = st.GetWallet(ctx, user)
				if w.Reserved != 0 || w.Balance != 1000-done.TotalCost {
					t.Fatalf("settled wallet %+v, cost %.2f", w, done.TotalCost)
				}
				var wg sync.WaitGroup
				errs := make(chan error, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, e := st.SettleSession(ctx, store.SettleInput{SessionID: session.ID, State: store.SessionTelemetry{KWhDelivered: 999}})
						errs <- e
					}()
				}
				wg.Wait()
				close(errs)
				for e := range errs {
					if e != nil {
						t.Fatal(e)
					}
				}
				var count int
				st.DB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE session_id=$1`, session.ID).Scan(&count)
				if count != 1 {
					t.Fatalf("settlements %d", count)
				}
			})
		}
	}
}

func TestQuoteOwnershipConsumptionAndFailure(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	other := quoteDriver(t, st, "SGD")
	uid := quoteEVSE(t, st, "SG")
	q, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.GetQuote(ctx, other, q.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign quote %v", err)
	}
	sess, err := accept(t, st, user, uid, q)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.FailSession(ctx, sess.ID, "FAULT"); err != nil {
		t.Fatal(err)
	}
	w, _ := st.GetWallet(ctx, user)
	if w.Reserved != 0 || w.Balance != 1000 {
		t.Fatalf("failed command left hold %+v", w)
	}
	if _, err = accept(t, st, user, uid, q); !errors.Is(err, store.ErrQuoteConsumed) {
		t.Fatalf("reused quote %v", err)
	}
	if _, err = st.DB.Exec(`UPDATE charging_quotes SET exchange_rate=9 WHERE id=$1`, q.ID); err == nil {
		t.Fatal("mutable quote")
	}
	if err = store.Migrate(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetWallet(ctx, user)
	if after.Balance != w.Balance {
		t.Fatal("migration changed wallet")
	}
}

func TestLockedRateAndHistoricalReceipt(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	uid := quoteEVSE(t, st, "SG")
	q, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := accept(t, st, user, uid, q)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AddExchangeRate(ctx, "SGD", "MYR", "4.99", "test-rate-change", time.Now()); err != nil {
		t.Fatal(err)
	}
	defer st.AddExchangeRate(ctx, "SGD", "MYR", q.ExchangeRate, "test-rate-restore", time.Now().Add(time.Second))
	var oldPrice string
	var oldUpdated time.Time
	st.DB.QueryRow(`SELECT price_per_kwh,last_updated FROM tariffs WHERE id=$1`, q.TariffID).Scan(&oldPrice, &oldUpdated)
	st.DB.Exec(`UPDATE tariffs SET price_per_kwh=99,last_updated=now() WHERE id=$1`, q.TariffID)
	defer st.DB.Exec(`UPDATE tariffs SET price_per_kwh=$2,last_updated=$3 WHERE id=$1`, q.TariffID, oldPrice, oldUpdated)
	done, err := st.SettleSession(ctx, store.SettleInput{SessionID: sess.ID, State: store.SessionTelemetry{KWhDelivered: 15}, BuildCDR: sessions.BuildCDR})
	if err != nil {
		t.Fatal(err)
	}
	if done.ExchangeRate != q.ExchangeRate || fmt.Sprint(done.TotalCost) != fmt.Sprint(31.05) {
		t.Fatalf("changed accepted pricing: %+v (rate %s)", done, q.ExchangeRate)
	}
	raw, err := st.CDRDocumentFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateCDRDocument(ctx, sess.ID, []byte(`{"id":"source","total_cost":{"before_taxes":999}}`)); err != nil {
		t.Fatal(err)
	}
	raw2, _ := st.CDRDocumentFor(ctx, sess.ID)
	if string(raw) != string(raw2) {
		t.Fatal("late CDR overwrote receipt")
	}
}

func TestStationGeographyAndSeedIdempotency(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	var count int
	st.DB.QueryRow(`SELECT count(*) FROM stations WHERE id LIKE 'stn_demo_%'`).Scan(&count)
	if count != 24 {
		t.Fatalf("simulated stations %d", count)
	}
	for _, country := range []string{"MY", "SG", "TH"} {
		stations, err := st.ListStations(ctx, store.StationFilter{CountryCode: country})
		if err != nil {
			t.Fatal(err)
		}
		ops := map[string]bool{}
		for _, station := range stations {
			if !strings.HasPrefix(station.ID, "stn_demo_") {
				continue
			}
			ops[station.CpoID] = true
			if station.CountryCode != country {
				t.Fatal("country filter mismatch")
			}
			if country == "MY" && (station.Latitude < 5.2 || station.Latitude > 5.5 || station.Longitude < 100.2 || station.Longitude > 100.5) {
				t.Fatalf("Penang coordinate %+v", station)
			}
			for _, connector := range station.Connectors {
				want := map[string]string{"MY": "MYR", "SG": "SGD", "TH": "THB"}[country]
				if connector.Currency != want {
					t.Fatalf("currency %s != %s", connector.Currency, want)
				}
			}
		}
		if len(ops) != 3 {
			t.Fatalf("%s operators %v", country, ops)
		}
	}
}

func TestStaleQuotesMissingRatesAndConcurrentAcceptance(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	uid := quoteEVSE(t, st, "SG")
	if _, err := st.CreateQuote(ctx, user, uid, "15", "missing-source"); !errors.Is(err, store.ErrRateUnavailable) {
		t.Fatalf("unavailable FX %v", err)
	}
	for _, rate := range []string{"0", "-1", "NaN", "Infinity", "not-a-number"} {
		if err := st.AddExchangeRate(ctx, "SGD", "MYR", rate, "invalid", time.Now()); err == nil {
			t.Fatalf("accepted invalid rate %s", rate)
		}
	}
	q, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	var price string
	var updated time.Time
	st.DB.QueryRow(`SELECT price_per_kwh,last_updated FROM tariffs WHERE id=$1`, q.TariffID).Scan(&price, &updated)
	if _, err = st.DB.Exec(`UPDATE tariffs SET price_per_kwh=price_per_kwh+0.01,last_updated=clock_timestamp() WHERE id=$1`, q.TariffID); err != nil {
		t.Fatal(err)
	}
	if _, err = accept(t, st, user, uid, q); !errors.Is(err, store.ErrQuoteChanged) {
		t.Fatalf("stale tariff %v", err)
	}
	if _, err = st.DB.Exec(`UPDATE tariffs SET price_per_kwh=$2,last_updated=$3 WHERE id=$1`, q.TariffID, price, updated); err != nil {
		t.Fatal(err)
	}
	first, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	accepted := make(chan *store.Session, 2)
	for _, quote := range []*store.ChargingQuote{first, second} {
		wg.Add(1)
		go func(q *store.ChargingQuote) {
			defer wg.Done()
			sess, e := accept(t, st, user, uid, q)
			if e == nil {
				accepted <- sess
			}
		}(quote)
	}
	wg.Wait()
	close(accepted)
	var winning *store.Session
	count := 0
	for sess := range accepted {
		winning = sess
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent starts accepted %d quotations", count)
	}
	var consumed int
	st.DB.QueryRow(`SELECT count(*) FROM charging_quotes WHERE user_id=$1 AND accepted_at IS NOT NULL`, user).Scan(&consumed)
	if consumed != 1 {
		t.Fatalf("consumed %d quotes", consumed)
	}
	if err = st.FailSession(ctx, winning.ID, "CANCELLED"); err != nil {
		t.Fatal(err)
	}
	wallet, _ := st.GetWallet(ctx, user)
	if wallet.Reserved != 0 || wallet.Balance != 1000 {
		t.Fatalf("reservation release %+v", wallet)
	}
}

func TestDebitSeesHoldCommittedWhileWaitingForWalletLock(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	uid := quoteEVSE(t, st, "SG")
	q, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := accept(t, st, user, uid, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB.Exec(`UPDATE wallet_reservations SET status='RELEASED' WHERE session_id=$1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	locker, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback()
	var walletID string
	if err = locker.QueryRow(`SELECT id FROM wallets WHERE user_id=$1 FOR UPDATE`, user).Scan(&walletID); err != nil {
		t.Fatal(err)
	}
	if _, err = locker.Exec(`UPDATE wallet_reservations SET status='HELD' WHERE session_id=$1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	debit, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer debit.Rollback()
	var pid int
	if err = debit.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := store.DebitWallet(ctx, debit, user, 10, "concurrent-hold"); result <- e }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err = st.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debit did not wait for wallet lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = locker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, store.ErrInsufficientBalance) {
			t.Fatalf("debit ignored newly committed hold: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("debit remained blocked")
	}
}

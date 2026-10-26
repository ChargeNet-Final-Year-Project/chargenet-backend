package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

func restoreLatestRate(t *testing.T, st *store.Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := st.AddExchangeRate(context.Background(), "SGD", "MYR", "3.45", "test-restore", time.Now()); err != nil {
			t.Error(err)
		}
	})
}

func TestPricingRevalidationAtStart(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	restoreLatestRate(t, st)
	uid := quoteEVSE(t, st, "SG")
	for _, scenario := range []string{"unchanged", "same-value-new-record", "changed-value"} {
		t.Run(scenario, func(t *testing.T) {
			user := quoteDriver(t, st, "MYR")
			// Keep each case independent of other tests' appended demonstration rates.
			source := "revalidation-" + scenario
			if err := st.AddExchangeRate(ctx, "SGD", "MYR", "3.45", source, time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			q, err := st.CreateQuote(ctx, user, uid, "15", source)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "unchanged" {
				rate := "3.4500000000"
				if scenario == "changed-value" {
					rate = "3.55"
				}
				if err = st.AddExchangeRate(ctx, "SGD", "MYR", rate, source, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			// Other providers must not override an explicitly selected simulation source.
			if err = st.AddExchangeRate(ctx, "SGD", "MYR", "99", "unselected-source", time.Now()); err != nil {
				t.Fatal(err)
			}
			sess, err := accept(t, st, user, uid, q)
			if scenario == "changed-value" {
				if !errors.Is(err, store.ErrQuoteChanged) {
					t.Fatalf("changed FX accepted: %v", err)
				}
				wallet, _ := st.GetWallet(ctx, user)
				original, _ := st.GetQuote(ctx, user, q.ID)
				if wallet.Reserved != 0 || original.AcceptedAt != nil {
					t.Fatal("rejected pricing consumed estimate or held funds")
				}
				q, err = st.CreateQuote(ctx, user, uid, "15", source)
				if err != nil {
					t.Fatal(err)
				}
				if q.EstimatedWalletCost != "31.95" {
					t.Fatalf("updated estimated total %s", q.EstimatedWalletCost)
				}
				sess, err = accept(t, st, user, uid, q)
			}
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := st.GetQuote(ctx, user, q.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "same-value-new-record" && *accepted.ExchangeRateID == *q.ExchangeRateID {
				t.Fatal("accepted obsolete FX metadata")
			}
			if accepted.ExchangeRate != sess.ExchangeRate || accepted.RateSource != sess.RateSource {
				t.Fatal("session and accepted FX snapshot differ")
			}
			if err = st.FailSession(ctx, sess.ID, "TEST_COMPLETE"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyEstimateDeadlineNoLongerBlocksStart(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	uid := quoteEVSE(t, st, "MY")
	q, err := st.CreateQuote(ctx, user, uid, "15")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(q)
	if strings.Contains(string(raw), "expiresAt") {
		t.Fatal("estimate API still exposes a deadline")
	}
	// Reproduce an unaccepted historical estimate without mutating its snapshot.
	id := store.NewID("old_estimate")
	_, err = st.DB.Exec(`INSERT INTO charging_quotes
 (id,user_id,wallet_id,cpo_id,station_id,connector_id,tariff_id,tariff_updated_at,tariff_per_kwh,
 original_currency,wallet_currency,exchange_rate_id,exchange_rate,rate_source,estimated_kwh,
 estimated_original_cost,estimated_wallet_cost,budget,created_at,expires_at,rate_selection_source)
 SELECT $2,user_id,wallet_id,cpo_id,station_id,connector_id,tariff_id,tariff_updated_at,tariff_per_kwh,
 original_currency,wallet_currency,exchange_rate_id,exchange_rate,rate_source,estimated_kwh,
 estimated_original_cost,estimated_wallet_cost,budget,now()-interval '1 year',now()-interval '364 days',rate_selection_source
 FROM charging_quotes WHERE id=$1`, q.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	q.ID = id
	if _, err = accept(t, st, user, uid, q); err != nil {
		t.Fatalf("unchanged historical estimate rejected: %v", err)
	}
}

func TestRateCommittedWhileStartWaitsIsRevalidated(t *testing.T) {
	st := testutil.NewTestStore(t)
	restoreLatestRate(t, st)
	ctx := context.Background()
	user := quoteDriver(t, st, "MYR")
	uid := quoteEVSE(t, st, "SG")
	source := "concurrent-rate"
	if err := st.AddExchangeRate(ctx, "SGD", "MYR", "3.45", source, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	q, err := st.CreateQuote(ctx, user, uid, "15", source)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err = writer.Exec(`INSERT INTO exchange_rates(id,source_currency,target_currency,rate,effective_at,source) VALUES($1,'SGD','MYR',3.55,clock_timestamp(),$2)`, store.NewID("fx"), source); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := accept(t, st, user, uid, q); result <- e }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err = st.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='exchange_rates'::regclass AND mode='ShareLock' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start did not wait for rate append")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if !errors.Is(err, store.ErrQuoteChanged) {
			t.Fatalf("stale concurrent rate accepted: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("start remained blocked after append committed")
	}
}

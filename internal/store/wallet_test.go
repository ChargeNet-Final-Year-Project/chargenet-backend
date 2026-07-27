package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// TestTopUpWalletConcurrent fires 20 simultaneous top-ups and asserts that the
// balance and ledger stay consistent.
func TestTopUpWalletConcurrent(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	user, err := st.CreateUser(ctx, "Concurrent", "concurrent@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	const goroutines = 20
	const amount = 10.00

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.TopUpWallet(ctx, user.ID, amount, store.PaymentMaybankFPX); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent top-up: %v", err)
	}

	wallet, err := st.GetWallet(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if want := float64(goroutines) * amount; wallet.Balance != want {
		t.Fatalf("balance = %.2f, want %.2f", wallet.Balance, want)
	}

	txs, err := st.ListWalletTransactions(ctx, user.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListWalletTransactions: %v", err)
	}
	if len(txs) != goroutines {
		t.Fatalf("transaction count = %d, want %d", len(txs), goroutines)
	}
}

// TestDebitWalletTypes checks the typed failures DebitWallet exposes for the session manager's
// settlement step.
func TestDebitWalletTypes(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	user, err := st.CreateUser(ctx, "Debitor", "debitor@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := st.TopUpWallet(ctx, user.ID, 20.00, store.PaymentTouchNGo); err != nil {
		t.Fatalf("TopUpWallet: %v", err)
	}

	// Over-spending must return ErrInsufficientBalance, not a CHECK violation.
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := store.DebitWallet(ctx, tx, user.ID, 50.00, "cdr_large"); !errors.Is(err, store.ErrInsufficientBalance) {
		t.Fatalf("overspend err = %v, want ErrInsufficientBalance", err)
	}
	_ = tx.Rollback()

	// A zero amount is a no-op that reports the unchanged balance.
	tx, err = st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	balance, err := store.DebitWallet(ctx, tx, user.ID, 0, "")
	if err != nil || balance != 20.00 {
		t.Fatalf("zero debit = (%.2f, %v), want (20.00, nil)", balance, err)
	}
	_ = tx.Rollback()

	// A funded debit commits and lands in the ledger as CHARGING_DEBIT.
	tx, err = st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	balance, err = store.DebitWallet(ctx, tx, user.ID, 12.50, "cdr_ok")
	if err != nil {
		t.Fatalf("DebitWallet: %v", err)
	}
	if balance != 7.50 {
		t.Fatalf("balance = %.2f, want 7.50", balance)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	txs, err := st.ListWalletTransactions(ctx, user.ID, 50, 0)
	if err != nil {
		t.Fatalf("ListWalletTransactions: %v", err)
	}
	if len(txs) != 2 || txs[0].Type != store.TxTypeChargingDebit || txs[0].ReferenceID != "cdr_ok" {
		t.Fatalf("unexpected ledger %+v", txs)
	}
}

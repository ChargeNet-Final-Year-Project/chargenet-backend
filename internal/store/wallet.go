package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Wallet transaction types. Amounts are always stored and
// returned positive; the type determines the sign shown in the UI.
const (
	TxTypeTopUp         = "TOPUP"
	TxTypeChargingDebit = "CHARGING_DEBIT"
	TxStatusSuccess     = "SUCCESS"
)

// Simulated payment methods.
const (
	PaymentMaybankFPX = "Maybank FPX"
	PaymentTouchNGo   = "Touch 'n Go eWallet"
	PaymentCreditCard = "Credit Card"

	// PaymentWallet labels wallet-funded charging debits.
	PaymentWallet = "Wallet"
)

// PaymentMethods is the ordered list offered to the mobile top-up sheet.
var PaymentMethods = []string{PaymentMaybankFPX, PaymentTouchNGo, PaymentCreditCard}

// ValidPaymentMethod reports whether m is one of the simulated gateways.
func ValidPaymentMethod(m string) bool {
	for _, p := range PaymentMethods {
		if p == m {
			return true
		}
	}
	return false
}

// Wallet is the public wallet DTO: {id,userId,balance,currency}.
type Wallet struct {
	Reserved         float64 `json:"reserved"`
	AvailableBalance float64 `json:"availableBalance"`
	ID               string  `json:"id"`
	UserID           string  `json:"userId"`
	Balance          float64 `json:"balance"`
	Currency         string  `json:"currency"`
}

// WalletTransaction is the public ledger entry DTO.
type WalletTransaction struct {
	Currency      string    `json:"currency"`
	SessionID     *string   `json:"sessionId"`
	CDRID         *string   `json:"cdrId"`
	ID            string    `json:"id"`
	WalletID      string    `json:"walletId"`
	Type          string    `json:"type"`
	Amount        float64   `json:"amount"`
	PaymentMethod string    `json:"paymentMethod"`
	ReferenceID   string    `json:"referenceId"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
}

// GetWallet loads the wallet belonging to a driver.
func (s *Store) GetWallet(ctx context.Context, userID string) (*Wallet, error) {
	w := &Wallet{}
	err := s.DB.QueryRowContext(ctx,
		`SELECT id,user_id,balance,currency,
 coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0),
 balance-coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0)
 FROM wallets w WHERE user_id=$1`, userID).
		Scan(&w.ID, &w.UserID, &w.Balance, &w.Currency, &w.Reserved, &w.AvailableBalance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

// TopUpWallet credits the wallet and records a TOPUP transaction in a single
// database transaction. The simulated gateway reference is derived from the
// current unix timestamp ("SIM-<unix>").
func (s *Store) TopUpWallet(ctx context.Context, userID string, amount float64, method string) (*Wallet, error) {
	return s.TopUpWalletDecimal(ctx, userID, strconv.FormatFloat(amount, 'f', -1, 64), method)
}

func (s *Store) TopUpWalletDecimal(ctx context.Context, userID, amount, method string) (*Wallet, error) {
	refID := fmt.Sprintf("SIM-%d", time.Now().Unix())

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	w, err := creditWallet(ctx, tx, userID, amount)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wallet_transactions (id, wallet_id, type, amount, payment_method, reference_id, status, currency)
		 VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, (SELECT currency FROM wallets WHERE id=$2::varchar))`,
		NewID("wtx"), w.ID, TxTypeTopUp, amount, method, refID, TxStatusSuccess,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetWallet(ctx, userID)
}

// DebitWallet deducts amount from the driver's wallet inside the caller's
// transaction and records a CHARGING_DEBIT row. It returns the new balance.
//
// It never relies on the CHECK (balance >= 0) constraint: an under-funded
// wallet yields ErrInsufficientBalance instead of a constraint violation, so
// callers can map it to a clean 402.
//
// A non-positive amount is a no-op (a 0 kWh session still completes with a
// 0.00 cost and no ledger row).
func DebitWallet(ctx context.Context, tx *sql.Tx, userID string, amount float64, refID string) (float64, error) {
	return debitWalletDecimal(ctx, tx, userID, strconv.FormatFloat(amount, 'f', 2, 64), refID)
}

func debitWalletDecimal(ctx context.Context, tx *sql.Tx, userID, amount, refID string) (float64, error) {
	var walletID string
	var balance float64
	var permitted, zero bool
	err := tx.QueryRowContext(ctx,
		`SELECT id,balance FROM wallets WHERE user_id=$1 FOR UPDATE`, userID).
		Scan(&walletID, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	// Use a new statement snapshot after acquiring the wallet lock: a hold
	// inserted by the transaction we waited for must be visible even when
	// that transaction did not change the wallet's balance column.
	err = tx.QueryRowContext(ctx, `SELECT $2::numeric<=balance-coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0),$2::numeric<=0 FROM wallets w WHERE id=$1`, walletID, amount).Scan(&permitted, &zero)
	if err != nil {
		return 0, err
	}
	if zero {
		return balance, nil
	}
	if !permitted {
		return balance, ErrInsufficientBalance
	}

	if err := tx.QueryRowContext(ctx,
		`UPDATE wallets SET balance = balance - $1::numeric, updated_at = NOW()
		  WHERE user_id = $2 RETURNING balance`,
		amount, userID).Scan(&balance); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wallet_transactions (id, wallet_id, type, amount, payment_method, reference_id, status, currency)
		 VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, (SELECT currency FROM wallets WHERE id=$2::varchar))`,
		NewID("wtx"), walletID, TxTypeChargingDebit, amount, PaymentWallet, refID, TxStatusSuccess,
	); err != nil {
		return 0, err
	}
	return balance, nil
}

// ListWalletTransactions returns the driver's ledger, newest first.
func (s *Store) ListWalletTransactions(ctx context.Context, userID string, limit, offset int) ([]WalletTransaction, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT t.id, t.wallet_id, t.type, t.amount, t.payment_method, t.reference_id, t.status, t.created_at, t.currency, t.session_id, t.cdr_id
		   FROM wallet_transactions t
		   JOIN wallets w ON w.id = t.wallet_id
		  WHERE w.user_id = $1
		  ORDER BY t.created_at DESC, t.id DESC
		  LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []WalletTransaction{}
	for rows.Next() {
		var t WalletTransaction
		if err := rows.Scan(&t.ID, &t.WalletID, &t.Type, &t.Amount, &t.PaymentMethod,
			&t.ReferenceID, &t.Status, &t.CreatedAt, &t.Currency, &t.SessionID, &t.CDRID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// creditWallet adds amount to a wallet inside tx and returns the updated row.
func creditWallet(ctx context.Context, tx *sql.Tx, userID string, amount string) (*Wallet, error) {
	w := &Wallet{}
	err := tx.QueryRowContext(ctx,
		`UPDATE wallets SET balance = balance + round($1::numeric,2), updated_at = NOW()
		  WHERE user_id = $2
		 RETURNING id, user_id, balance, currency`, amount, userID).
		Scan(&w.ID, &w.UserID, &w.Balance, &w.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/store"
)

type walletDTO struct {
	ID       string  `json:"id"`
	UserID   string  `json:"userId"`
	Balance  float64 `json:"balance"`
	Currency string  `json:"currency"`
}

type txDTO struct {
	ID            string  `json:"id"`
	WalletID      string  `json:"walletId"`
	Type          string  `json:"type"`
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"paymentMethod"`
	ReferenceID   string  `json:"referenceId"`
	Status        string  `json:"status"`
}

// doJSONHeaders is doJSON plus arbitrary request headers (e.g. Idempotency-Key).
func doJSONHeaders(t *testing.T, h http.Handler, method, path, token string, extra map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func topUp(t *testing.T, h http.Handler, token string, amount float64, method string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, http.MethodPost, "/api/v1/wallet/topup", token,
		map[string]any{"amount": amount, "paymentMethod": method})
}

func TestWalletTopUpAndTransactions(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/wallet", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /wallet = %d (%s)", rec.Code, rec.Body)
	}
	if w := decode[walletDTO](t, rec); w.Balance != 0 || w.Currency != "MYR" || w.ID == "" {
		t.Fatalf("new wallet = %+v, want 0.00 MYR", w)
	}

	rec = topUp(t, h, token, 50, store.PaymentMaybankFPX)
	if rec.Code != http.StatusOK {
		t.Fatalf("topup = %d (%s)", rec.Code, rec.Body)
	}
	if w := decode[walletDTO](t, rec); w.Balance != 50.00 {
		t.Fatalf("balance after topup = %.2f, want 50.00", w.Balance)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/wallet/transactions", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("transactions = %d (%s)", rec.Code, rec.Body)
	}
	txs := decode[[]txDTO](t, rec)
	if len(txs) != 1 {
		t.Fatalf("transaction count = %d, want 1", len(txs))
	}
	if tx := txs[0]; tx.Type != store.TxTypeTopUp || tx.Amount != 50 || tx.Status != store.TxStatusSuccess {
		t.Fatalf("transaction = %+v", tx)
	}
	if !strings.HasPrefix(txs[0].ReferenceID, "SIM-") {
		t.Fatalf("referenceId = %q, want SIM- prefix", txs[0].ReferenceID)
	}
	if txs[0].PaymentMethod != store.PaymentMaybankFPX {
		t.Fatalf("paymentMethod = %q", txs[0].PaymentMethod)
	}
}

// TestWalletTopUpDemoDriver verifies the seeded demo wallet: the demo
// driver's balance moves 85.50 → 135.50 and one TOPUP row appears.
func TestWalletTopUpDemoDriver(t *testing.T) {
	h, st := newTestRouter(t)
	if err := st.SeedUsers(context.Background()); err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}

	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": store.DemoEmail, "password": store.DemoPassword,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("demo login = %d (%s)", rec.Code, rec.Body)
	}
	token := decode[authResp](t, rec).Token

	rec = topUp(t, h, token, 50, store.PaymentTouchNGo)
	if rec.Code != http.StatusOK {
		t.Fatalf("topup = %d (%s)", rec.Code, rec.Body)
	}
	if w := decode[walletDTO](t, rec); w.Balance != 135.50 {
		t.Fatalf("balance = %.2f, want 135.50", w.Balance)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/wallet/transactions", token, nil)
	txs := decode[[]txDTO](t, rec)
	if len(txs) != 1 || txs[0].Type != store.TxTypeTopUp || txs[0].Amount != 50 {
		t.Fatalf("ledger = %+v, want a single 50.00 TOPUP", txs)
	}
}

// TestWalletTopUpIdempotentReplay replays a top-up with the same
// Idempotency-Key and asserts the original response is echoed without a second
// credit.
func TestWalletTopUpIdempotentReplay(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)
	body := map[string]any{"amount": 50, "paymentMethod": store.PaymentCreditCard}
	headers := map[string]string{"Idempotency-Key": "topup-replay-1"}

	first := doJSONHeaders(t, h, http.MethodPost, "/api/v1/wallet/topup", token, headers, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first topup = %d (%s)", first.Code, first.Body)
	}
	second := doJSONHeaders(t, h, http.MethodPost, "/api/v1/wallet/topup", token, headers, body)
	if second.Code != http.StatusOK {
		t.Fatalf("replay = %d (%s)", second.Code, second.Body)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body %q != original %q", second.Body.String(), first.Body.String())
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay missing Idempotent-Replay header")
	}
	if w := decode[walletDTO](t, second); w.Balance != 50.00 {
		t.Fatalf("balance after replay = %.2f, want 50.00", w.Balance)
	}

	rec := doJSON(t, h, http.MethodGet, "/api/v1/wallet/transactions", token, nil)
	if txs := decode[[]txDTO](t, rec); len(txs) != 1 {
		t.Fatalf("transaction count = %d, want 1 (no double credit)", len(txs))
	}

	// A different key (or no key) is a genuine new top-up.
	rec = topUp(t, h, token, 25, store.PaymentCreditCard)
	if w := decode[walletDTO](t, rec); w.Balance != 75.00 {
		t.Fatalf("balance = %.2f, want 75.00", w.Balance)
	}
}

func TestWalletTopUpValidation(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	for _, tc := range []struct {
		name   string
		amount float64
	}{
		{"below minimum", 3},
		{"above maximum", 2500},
		{"too many decimals", 10.555},
	} {
		rec := topUp(t, h, token, tc.amount, store.PaymentMaybankFPX)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", tc.name, rec.Code)
		}
		if body := decode[errResp](t, rec); body.Error.Code != "VALIDATION_ERROR" || body.Error.Fields["amount"] == "" {
			t.Fatalf("%s: body = %s", tc.name, rec.Body)
		}
	}

	rec := topUp(t, h, token, 50, "Bitcoin")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown method status = %d, want 400", rec.Code)
	}
	if body := decode[errResp](t, rec); body.Error.Code != "INVALID_PAYMENT_METHOD" {
		t.Fatalf("code = %q, want INVALID_PAYMENT_METHOD", body.Error.Code)
	}
}

func TestWalletRequiresAuth(t *testing.T) {
	h, _ := newTestRouter(t)
	for _, path := range []string{"/api/v1/wallet", "/api/v1/wallet/transactions"} {
		if rec := doJSON(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d, want 401", path, rec.Code)
		}
	}
}

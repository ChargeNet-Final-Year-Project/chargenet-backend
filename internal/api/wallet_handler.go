package api

import (
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// Pagination bounds for the wallet ledger.
const (
	defaultTxLimit = 50
	maxTxLimit     = 200
)

// simulatedGatewayDelay mimics a real payment-gateway round-trip. It is aborted early when the client disconnects.
const simulatedGatewayDelay = 800 * time.Millisecond

// registerWalletRoutes installs the wallet endpoints.
// POST /wallet/topup additionally honours an optional Idempotency-Key header.
func registerWalletRoutes(mux *http.ServeMux, s *Server) {
	auth := RequireAuth(s.Cfg.JWTSecret)
	handle := func(h http.HandlerFunc) http.Handler { return Chain(h, auth) }

	mux.Handle("GET /api/v1/wallet", handle(s.handleGetWallet))
	mux.Handle("GET /api/v1/wallet/transactions", handle(s.handleListWalletTransactions))
	mux.Handle("POST /api/v1/wallet/topup",
		Chain(http.HandlerFunc(s.handleTopUpWallet), auth, Idempotency(s.Store, "wallet/topup")))
}

func (s *Server) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := s.Store.GetWallet(r.Context(), UserIDFrom(r.Context()))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your wallet.")
		return
	}
	WriteJSON(w, http.StatusOK, wallet)
}

type topUpRequest struct {
	Amount        json.Number `json:"amount"`
	PaymentMethod string      `json:"paymentMethod"`
}

func (s *Server) handleTopUpWallet(w http.ResponseWriter, r *http.Request) {
	var req topUpRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}

	fields := map[string]string{}
	amount, valid := new(big.Rat).SetString(req.Amount.String())
	if !valid || amount.Cmp(big.NewRat(5, 1)) < 0 || amount.Cmp(big.NewRat(2000, 1)) > 0 {
		fields["amount"] = "Top-up amount must be between 5 and 2,000 in your wallet currency."
	} else if !new(big.Rat).Mul(amount, big.NewRat(100, 1)).IsInt() {
		fields["amount"] = "Top-up amount may have at most 2 decimal places."
	}
	if len(fields) > 0 {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}
	if !store.ValidPaymentMethod(req.PaymentMethod) {
		WriteError(w, http.StatusBadRequest, CodeInvalidPaymentMethod, "Choose a supported payment method.")
		return
	}

	select {
	case <-r.Context().Done():
		return // client gave up; nothing to settle
	case <-time.After(simulatedGatewayDelay):
	}

	wallet, err := s.Store.TopUpWalletDecimal(r.Context(), UserIDFrom(r.Context()), req.Amount.String(), req.PaymentMethod)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not complete the top-up.")
		return
	}
	WriteJSON(w, http.StatusOK, wallet)
}

func (s *Server) handleListWalletTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, ok := parseBoundedInt(q.Get("limit"), defaultTxLimit, 1, maxTxLimit)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeValidation, "limit must be a positive number.")
		return
	}
	offset, ok := parseBoundedInt(q.Get("offset"), 0, 0, 1_000_000)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeValidation, "offset must be zero or greater.")
		return
	}

	txs, err := s.Store.ListWalletTransactions(r.Context(), UserIDFrom(r.Context()), limit, offset)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your transactions.")
		return
	}
	WriteJSON(w, http.StatusOK, txs)
}

// hasCentPrecision reports whether v carries at most two decimal places.
func hasCentPrecision(v float64) bool {
	return math.Abs(v*100-math.Round(v*100)) < 1e-6
}

// parseBoundedInt parses an optional integer query parameter, defaulting to def
// when absent and clamping values above max.
func parseBoundedInt(raw string, def, min, max int) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

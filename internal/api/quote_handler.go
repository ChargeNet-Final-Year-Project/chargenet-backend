package api

import (
	"errors"
	"github.com/chargenet/chargenet-backend/internal/store"
	"net/http"
	"strings"
)

func (s *Server) handleCreateQuote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EVSEUID      string `json:"evseUid"`
		EstimatedKWh string `json:"estimatedKwh"`
	}
	if err := DecodeJSON(r, &body); err != nil || strings.TrimSpace(body.EVSEUID) == "" {
		WriteError(w, 400, CodeValidation, "evseUid and an optional decimal-string estimatedKwh are required.")
		return
	}
	q, err := s.Store.CreateQuote(r.Context(), UserIDFrom(r.Context()), strings.ToUpper(strings.TrimSpace(body.EVSEUID)), body.EstimatedKWh, s.Cfg.ExchangeRateSource)
	if err != nil {
		writeQuoteError(w, err)
		return
	}
	WriteJSON(w, 201, q)
}

func (s *Server) handleGetQuote(w http.ResponseWriter, r *http.Request) {
	q, err := s.Store.GetQuote(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeQuoteError(w, err)
		return
	}
	WriteJSON(w, 200, q)
}

func writeQuoteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrQuoteRequired):
		WriteError(w, 400, "QUOTE_REQUIRED", err.Error())
	case errors.Is(err, store.ErrQuoteConsumed):
		WriteError(w, 409, "QUOTE_CONSUMED", err.Error())
	case errors.Is(err, store.ErrQuoteChanged):
		WriteError(w, 409, "QUOTE_CHANGED", err.Error())
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, 404, "QUOTE_NOT_FOUND", "The estimated cost or charger could not be found.")
	case errors.Is(err, store.ErrConnectorBusy):
		WriteError(w, 409, CodeConnectorUnavailable, "That connector is not available.")
	case errors.Is(err, store.ErrInsufficientBalance):
		WriteError(w, 402, CodeInsufficientBalance, "Available wallet funds are too low. Top up and review the estimated cost again.")
	case errors.Is(err, store.ErrRateUnavailable):
		WriteError(w, 422, "RATE_UNAVAILABLE", err.Error())
	case errors.Is(err, store.ErrPricingUnsupported):
		WriteError(w, 422, "TARIFF_UNSUPPORTED", err.Error())
	case errors.Is(err, store.ErrQuoteEnergy):
		WriteError(w, 400, CodeValidation, err.Error())
	default:
		WriteError(w, 500, CodeInternal, "The estimated cost could not be loaded. Please retry.")
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// SSE tuning: a `: keepalive` comment every 15 real seconds
// so nothing between the phone and the API reaps an idle stream, and a short
// budget for the terminal re-read that closes a stream whose last frame the
// fan-out had to drop.
const (
	sseKeepalive   = 15 * time.Second
	streamReadWait = 2 * time.Second
)

// registerSessionRoutes installs the charging endpoints of the API contract
//
// `start` and `stop` are replayable on top of being idempotent: the mobile sends
// an Idempotency-Key with start, so a retry after a dropped response returns the
// session it already opened instead of opening a second one.
func registerSessionRoutes(mux *http.ServeMux, s *Server) {
	auth := RequireAuth(s.Cfg.JWTSecret)
	handle := func(h http.HandlerFunc) http.Handler { return Chain(h, auth) }
	replayable := func(endpoint string, h http.HandlerFunc) http.Handler {
		return Chain(h, auth, Idempotency(s.Store, endpoint))
	}

	mux.Handle("POST /api/v1/quotes", handle(s.handleCreateQuote))
	mux.Handle("GET /api/v1/quotes/{id}", handle(s.handleGetQuote))
	mux.Handle("POST /api/v1/sessions/start", replayable("sessions/start", s.handleStartSession))
	mux.Handle("POST /api/v1/sessions/{id}/stop", replayable("sessions/stop", s.handleStopSession))
	mux.Handle("GET /api/v1/sessions/active", handle(s.handleActiveSession))
	mux.Handle("GET /api/v1/sessions/history", handle(s.handleSessionHistory))
	mux.Handle("GET /api/v1/sessions/{id}", handle(s.handleGetSession))
	mux.Handle("GET /api/v1/sessions/{id}/stream", handle(s.handleSessionStream))
}

// startSessionRequest is the `POST /sessions/start` body.
type startSessionRequest struct {
	QuoteID   string  `json:"quoteId"`
	EVSEUID   string  `json:"evseUid"`
	VehicleID string  `json:"vehicleId"`
	TargetSoC float64 `json:"targetSoc"`
}

// handleStartSession opens a charge. The orchestrator runs ordered
// validation; the 201 body is the session row itself, so the monitor screen has
// numbers to render before the first telemetry frame arrives.
func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	var body startSessionRequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "The request body could not be read.")
		return
	}

	if body.QuoteID == "" {
		writeQuoteError(w, store.ErrQuoteRequired)
		return
	}
	sess, err := s.Sessions.Start(r.Context(), UserIDFrom(r.Context()), sessions.StartRequest{
		QuoteID:   body.QuoteID,
		EVSEUID:   body.EVSEUID,
		VehicleID: body.VehicleID,
		TargetSoC: body.TargetSoC,
	})
	if err != nil {
		if errors.Is(err, store.ErrQuoteRequired) || errors.Is(err, store.ErrQuoteConsumed) || errors.Is(err, store.ErrQuoteChanged) || errors.Is(err, store.ErrRateUnavailable) || errors.Is(err, store.ErrPricingUnsupported) || errors.Is(err, store.ErrInsufficientBalance) || errors.Is(err, store.ErrNotFound) {
			writeQuoteError(w, err)
			return
		}
		writeSessionError(w, err, "Could not start charging.")
		return
	}
	WriteJSON(w, http.StatusCreated, sess)
}

// handleStopSession ends a charge. Stopping a session that already finished
// returns it unchanged, so a double tap or a retry settles nothing
// twice.
func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Sessions.Stop(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, err, "Could not stop charging.")
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

// handleActiveSession returns the driver's live session, or 404
// NO_ACTIVE_SESSION — which the mobile reads as "nothing is running".
func (s *Server) handleActiveSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Sessions.Active(r.Context(), UserIDFrom(r.Context()))
	if err != nil {
		writeSessionError(w, err, "Could not load your session.")
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

// handleGetSession returns one of the driver's own sessions.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.Sessions.Get(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, err, "Could not load that session.")
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

// handleSessionHistory pages the driver's finished sessions, newest first.
func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, ok := parseBoundedInt(q.Get("limit"), sessions.HistoryDefaultLimit, 1, sessions.HistoryMaxLimit)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeValidation, "limit must be a positive number.")
		return
	}
	offset, ok := parseBoundedInt(q.Get("offset"), 0, 0, 1_000_000)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeValidation, "offset must be zero or greater.")
		return
	}

	rows, err := s.Sessions.History(r.Context(), UserIDFrom(r.Context()), limit, offset)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your history.")
		return
	}
	if rows == nil {
		rows = []store.Session{} // the mobile decodes a body that is always an array
	}
	WriteJSON(w, http.StatusOK, rows)
}

// handleSessionStream is the live telemetry stream: one
// `event: telemetry` frame per tick carrying the whole ChargingSession JSON,
// then a final `event: completed`.
//
// Subscribe before reading the row so ticks remain buffered during the lookup.
// The owner-scoped lookup hides sessions belonging to another driver.
func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	sub, sess, err := s.Sessions.Subscribe(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, err, "Live telemetry is unavailable.")
		return
	}
	defer sub.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Live telemetry is unavailable.")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering so frames arrive as they are written instead of in
	// one lump when the stream finally closes.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	write := func(event string, sess store.Session) {
		payload, err := json.Marshal(sess)
		if err != nil {
			log.Printf("api: encode session frame: %v", err)
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		flusher.Flush()
	}

	// Seed the stream with the row the subscription started from: a client that
	// connects mid-charge renders numbers before the first tick, and one that
	// connects after the charge ended still gets its terminal frame.
	if sess.Terminal() {
		write("completed", *sess)
		return
	}
	write("telemetry", *sess)

	ctx := r.Context()
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case frame, open := <-sub.C:
			if !open {
				// The charge ended (or the process is shutting down) while this
				// client was behind and missed the terminal frame: re-read the
				// row so the stream still closes with `completed`.
				write("completed", s.settledRow(ctx, sess.ID, sess))
				return
			}
			if frame.Terminal {
				write("completed", frame.Session)
				return
			}
			write("telemetry", frame.Session)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// settledRow re-reads a finished session. The read is detached from the request
// context — a client that hung up while the charge was finishing must not leave
// the handler without a terminal frame — and bounded, so the handler cannot hang
// on the database either. The last row seen is the fallback.
func (s *Server) settledRow(ctx context.Context, sessionID string, last *store.Session) store.Session {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamReadWait)
	defer cancel()
	if row, err := s.Store.SessionByID(readCtx, sessionID); err == nil {
		return *row
	}
	return *last
}

// writeSessionError maps an orchestrator failure onto the the API contract
// status table.
func writeSessionError(w http.ResponseWriter, err error, fallback string) {
	var se *sessions.Error
	if errors.As(err, &se) {
		// INSUFFICIENT_BALANCE carries the two numbers the pre-charge check
		// renders ("Minimum RM 10.00 required" beside the balance).
		if se.Code == sessions.CodeInsufficientBalance {
			required, balance := 0.0, 0.0
			if se.Required != nil {
				required = *se.Required
			}
			if se.Balance != nil {
				balance = *se.Balance
			}
			WriteInsufficientBalance(w, required, balance)
			return
		}
		WriteError(w, sessionErrorStatus(se.Code), ErrorCode(se.Code), se.Message)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeSessionNotFound, "We couldn't find that session.")
		return
	}
	log.Printf("api: session request failed: %v", err)
	WriteError(w, http.StatusInternalServerError, CodeInternal, fallback)
}

// An unmapped orchestrator code is a programming error and surfaces as a 500.
func sessionErrorStatus(code string) int {
	switch code {
	case sessions.CodeValidation:
		return http.StatusBadRequest
	case sessions.CodeInsufficientBalance:
		return http.StatusPaymentRequired
	case sessions.CodeIncompatibleConnector:
		return http.StatusUnprocessableEntity
	case sessions.CodeCPOUnreachable:
		return http.StatusBadGateway
	case sessions.CodeEVSENotFound, sessions.CodeVehicleNotFound,
		sessions.CodeSessionNotFound, sessions.CodeNoActiveSession:
		return http.StatusNotFound
	case sessions.CodeConnectorUnavailable, sessions.CodeSessionAlreadyActive:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

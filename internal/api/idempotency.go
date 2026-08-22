package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// Idempotency-Key is the optional request header; Idempotent-Replay is set on
// replayed responses so callers can tell a stored response from a fresh one.
const (
	headerIdempotencyKey   = "Idempotency-Key"
	headerIdempotentReplay = "Idempotent-Replay"
)

// A duplicate request waits this long for the original to finish before
// reporting that the key is still in use (~4.5 s, comfortably longer than the
// simulated gateway delay).
const (
	idempotencyPollInterval = 30 * time.Millisecond
	idempotencyPollAttempts = 150
)

// Idempotency makes a POST handler replayable. The first request carrying an
// Idempotency-Key runs normally and its 2xx response is persisted; any later
// request with the same key (same driver + endpoint) gets the stored response
// back verbatim and never runs the handler again. Requests without the header
// pass straight through.
//
// Failed responses are not cached: the reservation is released so the client
// can retry the operation.
func Idempotency(st *store.Store, endpoint string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimSpace(r.Header.Get(headerIdempotencyKey))
			userID := UserIDFrom(r.Context())
			if key == "" || userID == "" {
				next.ServeHTTP(w, r)
				return
			}

			owned, err := st.ReserveIdempotency(r.Context(), key, userID, endpoint)
			if err != nil {
				WriteError(w, http.StatusInternalServerError, CodeInternal, "Something went wrong.")
				return
			}
			if !owned {
				replayIdempotent(w, r, st, key, userID, endpoint)
				return
			}

			buf := newBufferingResponseWriter()
			next.ServeHTTP(buf, r)
			status := buf.statusCode()
			body := buf.buf.Bytes()

			if status >= 200 && status < 300 && json.Valid(body) {
				if err := st.SaveIdempotency(r.Context(), key, userID, endpoint, status, body); err != nil {
					log.Printf("api: store idempotent response: %v", err)
				} else if stored, err := st.GetIdempotency(r.Context(), key, userID, endpoint); err == nil && len(stored.Body) > 0 {
					// JSONB normalises key order and spacing, so echo the stored
					// bytes on the first response too: a replay is then
					// byte-for-byte identical to the original.
					body = stored.Body
				}
			} else {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
				if err := st.ReleaseIdempotency(releaseCtx, key, userID, endpoint); err != nil {
					log.Printf("api: release idempotency key: %v", err)
				}
				cancel()
			}

			buf.flushTo(w, body)
		})
	}
}

// replayIdempotent waits for the original request to finish, then echoes the
// stored response.
func replayIdempotent(w http.ResponseWriter, r *http.Request, st *store.Store, key, userID, endpoint string) {
	ctx := r.Context()
	for attempt := 0; attempt < idempotencyPollAttempts; attempt++ {
		rec, err := st.GetIdempotency(ctx, key, userID, endpoint)
		switch {
		case err == nil && rec.Status != store.IdempotencyInFlightStatus:
			w.Header().Set(headerIdempotentReplay, "true")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(rec.Status)
			_, _ = w.Write(rec.Body)
			return
		case err != nil && !errors.Is(err, store.ErrNotFound):
			WriteError(w, http.StatusInternalServerError, CodeInternal, "Something went wrong.")
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(idempotencyPollInterval):
		}
	}
	WriteError(w, http.StatusConflict, CodeRequestInProgress,
		"A request with this Idempotency-Key is still in progress.")
}

// bufferingResponseWriter captures a handler's status, headers and body so the
// idempotency middleware can persist and replay them.
type bufferingResponseWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func newBufferingResponseWriter() *bufferingResponseWriter {
	return &bufferingResponseWriter{header: http.Header{}}
}

func (b *bufferingResponseWriter) Header() http.Header { return b.header }

func (b *bufferingResponseWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferingResponseWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// Flush is intentionally a no-op: the body must stay in memory until it has
// been persisted. Idempotent endpoints never stream.
func (b *bufferingResponseWriter) Flush() {}

func (b *bufferingResponseWriter) statusCode() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

// flushTo replays the captured response into the real ResponseWriter, using
// body when the caller substituted it (see the JSONB normalisation note above).
func (b *bufferingResponseWriter) flushTo(w http.ResponseWriter, body []byte) {
	dst := w.Header()
	for k, vv := range b.header {
		dst[k] = append([]string(nil), vv...)
	}
	w.WriteHeader(b.statusCode())
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

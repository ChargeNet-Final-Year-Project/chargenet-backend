package api

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/auth"
)

type ctxKey string

const (
	ctxUserID ctxKey = "userID"
	ctxRole   ctxKey = "role"
	ctxCPOID  ctxKey = "cpoID"
)

// UserIDFrom returns the authenticated driver id stored by RequireAuth.
func UserIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxUserID).(string)
	return v
}

// RoleFrom / CPOIDFrom expose the portal claims set by RequirePortal.
func RoleFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRole).(string)
	return v
}

func CPOIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxCPOID).(string)
	return v
}

// statusRecorder captures the status code and bytes written for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so SSE streaming works through the
// middleware chain.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Chain applies middleware in the order given: Chain(h, mw1, mw2) runs
// mw1(mw2(h)) — mw1 is outermost.
func Chain(h http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}

// Recover converts panics into a 500 INTERNAL error response.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("api: panic %s %s: %v", r.Method, r.URL.Path, rec)
				WriteError(w, http.StatusInternalServerError, CodeInternal, "Something went wrong.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LogRequests logs method, path, status and duration for every request.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Printf("%s %s -> %d (%s, %dB)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), rec.bytes)
	})
}

// CORS allows the configured portal origin to make credentialed (cookie)
// requests, while leaving `*` for everything else (native mobile clients, curl,
// the OCPI emulators). A browser fetch with credentials rejects
// `Access-Control-Allow-Origin: *`, so when the request's Origin matches
// PORTAL_ORIGIN we echo it and allow credentials; otherwise we fall back to the
// open dev policy. Preflight OPTIONS is short-circuited.
func CORS(portalOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			h.Set("Access-Control-Max-Age", "600")
			origin := r.Header.Get("Origin")
			if origin != "" && origin == portalOrigin {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Add("Vary", "Origin")
			} else {
				h.Set("Access-Control-Allow-Origin", "*")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth validates the mobile bearer token (aud="mobile") and injects the
// driver id into the request context.
func RequireAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := parseBearer(secret, r, auth.AudMobile)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "Authentication required.")
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserID, claims.Subject)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func parseBearer(secret string, r *http.Request, audience string) (*auth.Claims, error) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return nil, auth.ErrInvalidToken
	}
	return auth.ParseToken(secret, strings.TrimSpace(header[len(prefix):]), audience)
}

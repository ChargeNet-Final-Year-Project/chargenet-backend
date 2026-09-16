package ocpi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// tokenKind selects which credential a route accepts.
type tokenKind int

const (
	// tokenVersions guards GET versions and version details: the endpoints a CPO
	// reads with its Token A while registering and re-reads with Token C
	// afterwards.
	tokenVersions tokenKind = iota
	// tokenRegistration guards `POST /credentials`, which only a fresh Token A
	// opens — it is spent by the exchange.
	tokenRegistration
	// tokenOperational guards every other module: Token C only.
	tokenOperational
)

// ctxKey scopes the context values this package stores.
type ctxKey string

// ctxCPO is the key the authenticated partner is attached under.
const ctxCPO ctxKey = "ocpiCPO"

// CPOFrom returns the partner that authenticated this request, or nil when the
// handler was reached without the middleware.
func CPOFrom(ctx context.Context) *store.CpoAccount {
	cpo, _ := ctx.Value(ctxCPO).(*store.CpoAccount)
	return cpo
}

// authorize validates `Authorization: Token <token>` and attaches the resolved
// CPO to the request context. A missing, unknown or spent token is answered with
// HTTP 401 and OCPI status 2001; a database failure with 500/3001, so a CPO can
// tell "your token is wrong" from "ChargeNet is broken".
func (s *Server) authorize(kind tokenKind) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := tokenFrom(r.Header.Get("Authorization"))
			if !ok {
				Fail(w, http.StatusUnauthorized, StatusUnauthorized, "Missing or malformed Authorization header.")
				return
			}

			cpo, err := s.resolve(r.Context(), kind, raw)
			if err != nil {
				log.Printf("ocpi: resolving token for %s %s: %v", r.Method, r.URL.Path, err)
				Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not verify the token.")
				return
			}
			if cpo == nil {
				Fail(w, http.StatusUnauthorized, StatusUnauthorized, "Unknown or expired token.")
				return
			}

			logCorrelation(cpo, r)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxCPO, cpo)))
		})
	}
}

// resolve finds the CPO a presented token belongs to, trying the token verbatim
// before any base64 decoding of it.
func (s *Server) resolve(ctx context.Context, kind tokenKind, raw string) (*store.CpoAccount, error) {
	for _, token := range tokenCandidates(raw) {
		if kind != tokenRegistration {
			cpo, err := lookupToken(ctx, s.Store.GetCPOByTokenC, token)
			if err != nil {
				return nil, err
			}
			if cpo != nil {
				return cpo, nil
			}
		}
		if kind != tokenOperational {
			cpo, err := lookupToken(ctx, s.Store.GetCPOByTokenA, token)
			if err != nil {
				return nil, err
			}
			if cpo != nil {
				return cpo, nil
			}
		}
	}
	return nil, nil
}

// lookupToken runs one repository lookup, mapping "no such row" onto "this token
// belongs to nobody" while letting real failures through.
func lookupToken(ctx context.Context, get func(context.Context, string) (*store.CpoAccount, error), token string) (*store.CpoAccount, error) {
	cpo, err := get(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cpo, nil
}

// tokenFrom parses the OCPI authorization scheme — `Token <token>`, not the
// mobile API's `Bearer <jwt>`.
func tokenFrom(header string) (string, bool) {
	const prefix = "Token "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// logCorrelation records the correlation headers a CPO sends so
// one push can be traced back through the logs.
func logCorrelation(cpo *store.CpoAccount, r *http.Request) {
	requestID := r.Header.Get("X-Request-ID")
	correlationID := r.Header.Get("X-Correlation-ID")
	if requestID == "" && correlationID == "" {
		return
	}
	log.Printf("ocpi: %s %s cpo=%s party=%s/%s request-id=%q correlation-id=%q",
		r.Method, r.URL.Path, cpo.ID, cpo.CountryCode, cpo.PartyID, requestID, correlationID)
}

// Package ocpi — see types.go for the overview.
package ocpi

import (
	"net/http"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// Server is ChargeNet's OCPI surface: the registration endpoints a CPO calls
// first, the credential exchange, and — from OCPI receivers — the module receivers.
type Server struct {
	Cfg   config.Config
	Store *store.Store
	// Sink is where the receivers hand the pushes about a running charge
	//. It is nil until the OCPI gateway is wired, and
	// the receivers then persist what they own and simply accept the rest.
	Sink TelemetrySink
	// Commands is where a CPO's asynchronous command result is handed over
	//. It is nil until the OCPI gateway is wired too.
	Commands CommandResultSink
}

// New builds the OCPI server. It holds no state of its own: every request is
// resolved against the database, so a restart never invalidates a Token C.
func New(cfg config.Config, st *store.Store) *Server {
	return &Server{Cfg: cfg, Store: st}
}

// Register installs the eMSP endpoints on mux.
//
// OCPI credentials owns versions + credentials: what a CPO needs *before* it can be talked to.
// The module receivers (locations, tariffs, sessions, cdrs, commands) arrive in
// OCPI receivers and authenticate with [tokenOperational] — Token C.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/ocpi/emsp/2.2.1", LegacyAdapter(mux, "/ocpi/emsp/2.2.1", VersionURLPath))
	mux.Handle("/ocpi/emsp/2.2.1/", LegacyAdapter(mux, "/ocpi/emsp/2.2.1", VersionURLPath))
	// Versions: open to a fresh Token A (how registration starts) and to
	// Token C (a registered partner re-reads its version details). The
	// trailing-slash form is what the versions response advertises; the bare
	// path is registered too so ServeMux never answers a redirect a CPO client
	// would have to follow.
	mux.Handle("GET "+VersionsURLPath, s.authorize(tokenVersions)(http.HandlerFunc(s.handleVersions)))
	mux.Handle("GET "+VersionURLPath+"/", s.authorize(tokenVersions)(http.HandlerFunc(s.handleVersionDetails)))
	mux.Handle("GET "+VersionURLPath, s.authorize(tokenVersions)(http.HandlerFunc(s.handleVersionDetails)))

	// Credentials: POST completes the exchange (Token A), the rest is the
	// registered partner's own credentials (Token C).
	mux.Handle("POST "+CredentialsPath, s.authorize(tokenRegistration)(http.HandlerFunc(s.handlePostCredentials)))
	mux.Handle("GET "+CredentialsPath, s.authorize(tokenOperational)(http.HandlerFunc(s.handleGetCredentials)))
	mux.Handle("PUT "+CredentialsPath, s.authorize(tokenOperational)(http.HandlerFunc(s.handlePutCredentials)))
	mux.Handle("DELETE "+CredentialsPath, s.authorize(tokenOperational)(http.HandlerFunc(s.handleDeleteCredentials)))

	// The modules OCPI receivers fills in: authenticated today, answered 3001 until then.
	s.registerModules(mux)
}

// Handler returns the OCPI endpoints on a mux of their own. Tests use it to
// exercise OCPI without the mobile API or the single-flow middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// absolute renders an OCPI path as the URL a CPO should call, using the
// configured PUBLIC_BASE_URL.
func (s *Server) absolute(path string) string {
	return s.Cfg.PublicBaseURL + path
}

// credentials is ChargeNet's half of the credentials module: the token the CPO
// must use from now on (Token C) and where ChargeNet's OCPI surface lives. It is
// the payload of both `POST /credentials` and `GET /credentials`.
func (s *Server) credentials(token string) Credentials {
	return Credentials{
		Token: token,
		URL:   s.absolute(VersionsURLPath),
		Roles: []CredentialsRole{{
			Role:            RoleEMSP,
			PartyID:         EMSPPartyID,
			CountryCode:     EMSPCountryCode,
			BusinessDetails: BusinessDetails{Name: EMSPName},
		}},
	}
}

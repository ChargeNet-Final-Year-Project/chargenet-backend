package ocpi

import (
	"net/http"
	"strings"
)

// handleVersions answers `GET /ocpi/emsp/versions`: the single version ChargeNet
// speaks and where its version details — the second half of the registration
// handshake — live.
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	OK(w, http.StatusOK, []Version{{
		Version: VersionNumber,
		URL:     s.absolute(VersionURLPath) + "/",
	}})
}

// handleVersionDetails answers `GET /ocpi/emsp/2.3.0/` with the module list.
//
// The route is registered as a subtree so ServeMux sends every path under it
// here; the equality check below keeps an unknown path from being answered with
// the endpoint list — a CPO that typo'd its URL deserves a 404, not a 200 that
// says nothing. The modules themselves are routed before this handler ever sees
// them (see [Server.registerModules]); only paths that are no module at all
// reach the guard.
func (s *Server) handleVersionDetails(w http.ResponseWriter, r *http.Request) {
	if trimTrailingSlash(r.URL.Path) != VersionURLPath {
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
		return
	}
	OK(w, http.StatusOK, VersionDetails{Version: VersionNumber, Endpoints: s.endpoints()})
}

// endpoints is the module list
//
// ChargeNet is the eMSP, so every module it serves is a RECEIVER: CPOs push
// their locations, tariffs, sessions and CDRs here, and post the asynchronous
// results of the commands ChargeNet sent them to the commands endpoint.
func (s *Server) endpoints() []Endpoint {
	out := make([]Endpoint, 0, len(receiverModules)+1)
	add := func(module string) {
		out = append(out, Endpoint{
			Identifier: module,
			Role:       InterfaceReceiver,
			URL:        s.absolute(VersionURLPath + "/" + module),
		})
	}
	// Credentials is the one module OCPI credentials implements itself; the rest are the
	// receivers of [receiverModules], advertised before OCPI receivers writes them.
	add(ModuleCredentials)
	for _, module := range receiverModules {
		add(module)
	}
	return out
}

// trimTrailingSlash drops one trailing slash so `/2.3.0/` and `/2.3.0` compare
// equal without turning `/` into an empty path.
func trimTrailingSlash(p string) string {
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		return p[:len(p)-1]
	}
	return p
}

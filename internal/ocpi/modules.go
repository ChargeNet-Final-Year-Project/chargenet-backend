package ocpi

import "net/http"

// receiverModules receive network data and asynchronous command results.
// Credentials has a separate handler. Ordering matches version discovery.
var receiverModules = []string{
	ModuleLocations,
	ModuleTariffs,
	ModuleSessions,
	ModuleCDRs,
	ModuleCommands,
}

// Every advertised module returns an OCPI envelope, including unsupported methods.
var moduleMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
}

// Every advertised path returns an envelope. Invalid tokens receive OCPI 2001;
// authenticated unsupported pushes must still report that nothing was stored.
//
// Each module is registered on its own path and on the subtree its identifiers
// live under (`/locations/{cc}/{pid}/{loc}`), per method, so the patterns nest
// inside the version-details subtree instead of conflicting with it.
//
// Receiver registration replaces each placeholder while keeping authentication.
func (s *Server) registerModules(mux *http.ServeMux) {
	for _, module := range receiverModules {
		path := VersionURLPath + "/" + module
		h := s.authorize(tokenOperational)(s.moduleReceiver(module))
		for _, method := range moduleMethods {
			mux.Handle(method+" "+path, h)
			mux.Handle(method+" "+path+"/", h)
		}
	}
}

// moduleReceiver is the receiver of one module
func (s *Server) moduleReceiver(module string) http.HandlerFunc {
	switch module {
	case ModuleLocations:
		return s.handleLocations
	case ModuleTariffs:
		return s.handleTariffs
	case ModuleSessions:
		return s.handleSessions
	case ModuleCDRs:
		return s.handleCDRs
	case ModuleCommands:
		return s.handleCommands
	default:
		return s.handleModulePending(module)
	}
}

// handleModulePending answers an authenticated call to a module whose receiver
// OCPI receivers has not written yet. The answer is deliberately not a success envelope: a
// CPO must not believe data was ingested when nothing was. 3001 is the spec's
// "the server cannot process this" code.
func (s *Server) handleModulePending(module string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Fail(w, http.StatusNotImplemented, StatusUnableToUseAPI,
			"The "+module+" module is not available on this server yet.")
	}
}

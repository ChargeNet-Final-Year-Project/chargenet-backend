package api

import (
	"net/http"
)

func registerPortalRoutes(mux *http.ServeMux, s *Server) {
	registerPortalAuthRoutes(mux, s)
	registerPortalCPORoutes(mux, s)
	registerPortalAdminRoutes(mux, s)
}

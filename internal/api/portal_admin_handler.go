package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// registerPortalAdminRoutes installs the super-admin endpoints. RequirePortal gates them on SUPER_ADMIN, so a CPO-role console can never
// reach them.
func registerPortalAdminRoutes(mux *http.ServeMux, s *Server) {
	admin := func(h http.HandlerFunc) http.Handler {
		return Chain(h, s.RequirePortal(store.PortalRoleSuperAdmin))
	}
	mux.Handle("GET /admin/api/admin/cpos", admin(s.handlePortalAdminCPOs))
	mux.Handle("PUT /admin/api/admin/cpos/{id}/status", admin(s.handlePortalAdminSetStatus))
	mux.Handle("GET /admin/api/admin/overview", admin(s.handlePortalAdminOverview))
}

// handlePortalAdminCPOs lists every partner with its network size and today's
// numbers.
func (s *Server) handlePortalAdminCPOs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListAllCPOsAdmin(r.Context(), reportCurrencyParam(r))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load the partners.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"cpos": rows})
}

// portalStatusRequest is the `PUT /admin/api/admin/cpos/{id}/status` body.
type portalStatusRequest struct {
	Status         string   `json:"status"`
	PlatformFeePct *float64 `json:"platformFeePct"`
}

// handlePortalAdminSetStatus approves, suspends or re-pends a partner and,
// optionally, edits its platform fee.
func (s *Server) handlePortalAdminSetStatus(w http.ResponseWriter, r *http.Request) {
	var body portalStatusRequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	status := strings.ToUpper(strings.TrimSpace(body.Status))
	switch status {
	case store.CPOStatusActive, store.CPOStatusSuspended, store.CPOStatusPending:
	default:
		WriteError(w, http.StatusBadRequest, CodeValidation, "Status must be ACTIVE, SUSPENDED or PENDING.")
		return
	}
	if body.PlatformFeePct != nil && (*body.PlatformFeePct < 0 || *body.PlatformFeePct > 20) {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Platform fee must be between 0 and 20 percent.")
		return
	}

	id := r.PathValue("id")
	if err := s.Store.SetCPOStatus(r.Context(), id, status, body.PlatformFeePct); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusNotFound, CodeStationNotFound, "No such partner.")
			return
		}
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not update the partner.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"id": id, "status": status, "platformFeePct": body.PlatformFeePct})
}

// handlePortalAdminOverview returns the network-wide KPIs and 7-day chart.
func (s *Server) handlePortalAdminOverview(w http.ResponseWriter, r *http.Request) {
	view, err := s.Store.AdminOverview(r.Context(), reportCurrencyParam(r))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load the overview.")
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

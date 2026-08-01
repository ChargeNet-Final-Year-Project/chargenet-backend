package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// registerStationRoutes installs the discovery endpoints.
// All of them require a mobile bearer token.
func registerStationRoutes(mux *http.ServeMux, s *Server) {
	auth := RequireAuth(s.Cfg.JWTSecret)
	handle := func(h http.HandlerFunc) http.Handler { return Chain(h, auth) }

	mux.Handle("GET /api/v1/cpos", handle(s.handleListCpos))
	mux.Handle("GET /api/v1/stations", handle(s.handleListStations))
	mux.Handle("GET /api/v1/stations/regions", handle(s.handleStationRegions))
	mux.Handle("GET /api/v1/stations/{id}", handle(s.handleGetStation))
	mux.Handle("GET /api/v1/evse/{evseUid}", handle(s.handleGetEVSE))
}

func (s *Server) handleListCpos(w http.ResponseWriter, r *http.Request) {
	cpos, err := s.Store.ListCpos(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load operators.")
		return
	}
	WriteJSON(w, http.StatusOK, cpos)
}

func (s *Server) handleListStations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.StationFilter{
		Region:        strings.TrimSpace(q.Get("region")),
		PowerType:     strings.ToUpper(strings.TrimSpace(q.Get("powerType"))),
		CountryCode:   strings.ToUpper(q.Get("country")),
		Query:         q.Get("query"),
		ConnectorType: q.Get("connectorType"),
		CpoID:         q.Get("cpoId"),
		AvailableOnly: q.Get("availableOnly") == "true",
	}
	if filter.CountryCode != "" && filter.CountryCode != "MY" && filter.CountryCode != "SG" && filter.CountryCode != "TH" {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Choose MY, SG or TH.")
		return
	}
	if filter.Region != "" && filter.CountryCode == "" {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Choose a country before a state or region.")
		return
	}
	if filter.PowerType != "" && filter.PowerType != "AC" && filter.PowerType != "DC" {
		WriteError(w, http.StatusBadRequest, CodeValidation, "powerType must be AC or DC.")
		return
	}

	if v := q.Get("minPower"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			WriteError(w, http.StatusBadRequest, CodeValidation, "minPower must be a number.")
			return
		}
		filter.MinPowerKW = f
	}
	if v := q.Get("maxPower"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			WriteError(w, http.StatusBadRequest, CodeValidation, "maxPower must be a positive number.")
			return
		}
		filter.MaxPowerKW = f
	}
	if filter.MaxPowerKW > 0 && filter.MinPowerKW > filter.MaxPowerKW {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Minimum power cannot exceed maximum power.")
		return
	}

	lat, latOK := parseFloatPtr(q.Get("lat"))
	lng, lngOK := parseFloatPtr(q.Get("lng"))
	if latOK && lngOK {
		filter.Lat, filter.Lng = lat, lng
		if radius, ok := parseFloatPtr(q.Get("radiusKm")); ok {
			filter.RadiusKm = radius
		}
	}

	stations, err := s.Store.ListStations(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load stations.")
		return
	}
	WriteJSON(w, http.StatusOK, stations)
}

func (s *Server) handleStationRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.Store.ListStationRegions(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load regions.")
		return
	}
	WriteJSON(w, http.StatusOK, regions)
}

func (s *Server) handleGetStation(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetStation(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCpoInactive) {
		// A suspended partner's station reads as gone, by link as well as in
		// the list.
		WriteError(w, http.StatusNotFound, CodeStationNotFound, "Station not found.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load the station.")
		return
	}
	WriteJSON(w, http.StatusOK, st)
}

func (s *Server) handleGetEVSE(w http.ResponseWriter, r *http.Request) {
	st, conn, err := s.Store.GetEVSE(r.Context(), r.PathValue("evseUid"))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCpoInactive) {
		// A suspended partner's EVSE reads as gone, the same way its station
		// does in discovery.
		WriteError(w, http.StatusNotFound, CodeEVSENotFound, "We couldn't find that charger.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not look up the charger.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"station": st, "connector": conn})
}

func parseFloatPtr(s string) (*float64, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, false
	}
	return &f, true
}

package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// registerVehicleRoutes installs the vehicle CRUD endpoints.
// Every one of them requires a mobile bearer token and is scoped to the driver.
func registerVehicleRoutes(mux *http.ServeMux, s *Server) {
	auth := RequireAuth(s.Cfg.JWTSecret)
	handle := func(h http.HandlerFunc) http.Handler { return Chain(h, auth) }

	mux.Handle("GET /api/v1/vehicles", handle(s.handleListVehicles))
	mux.Handle("POST /api/v1/vehicles", handle(s.handleCreateVehicle))
	mux.Handle("PUT /api/v1/vehicles/{id}", handle(s.handleUpdateVehicle))
	mux.Handle("DELETE /api/v1/vehicles/{id}", handle(s.handleDeleteVehicle))
}

// vehicleRequest is the create/update payload. ID and UserID are accepted but
// deliberately ignored: ownership always comes from the
// bearer token, never the body.
type vehicleRequest struct {
	ID              string  `json:"id"`
	UserID          string  `json:"userId"`
	Name            string  `json:"name"`
	LicensePlate    string  `json:"licensePlate"`
	ConnectorType   string  `json:"connectorType"`
	BatteryCapacity float64 `json:"batteryCapacity"`
	IsDefault       bool    `json:"isDefault"`
}

// toInput validates the payload, returning per-field messages on failure.
func (req vehicleRequest) toInput() (store.VehicleInput, map[string]string) {
	fields := map[string]string{}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		fields["name"] = "Give your vehicle a name."
	}

	plate := store.NormalizeLicensePlate(req.LicensePlate)
	if plate == "" {
		fields["licensePlate"] = "Licence plate is required."
	}

	connector := strings.ToUpper(strings.TrimSpace(req.ConnectorType))
	if !store.ValidConnectorStandard(connector) {
		fields["connectorType"] = "Choose a supported connector type."
	}

	if req.BatteryCapacity < store.MinBatteryCapacity || req.BatteryCapacity > store.MaxBatteryCapacity {
		fields["batteryCapacity"] = "Battery capacity must be between 10 and 250 kWh."
	}

	if len(fields) > 0 {
		return store.VehicleInput{}, fields
	}
	return store.VehicleInput{
		Name:            name,
		LicensePlate:    plate,
		ConnectorType:   connector,
		BatteryCapacity: store.Round2(req.BatteryCapacity),
		IsDefault:       req.IsDefault,
	}, nil
}

func (s *Server) handleListVehicles(w http.ResponseWriter, r *http.Request) {
	vehicles, err := s.Store.ListVehicles(r.Context(), UserIDFrom(r.Context()))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your vehicles.")
		return
	}
	WriteJSON(w, http.StatusOK, vehicles)
}

func (s *Server) handleCreateVehicle(w http.ResponseWriter, r *http.Request) {
	var req vehicleRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	input, fields := req.toInput()
	if fields != nil {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}

	vehicle, err := s.Store.CreateVehicle(r.Context(), UserIDFrom(r.Context()), input)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not save the vehicle.")
		return
	}
	WriteJSON(w, http.StatusCreated, vehicle)
}

func (s *Server) handleUpdateVehicle(w http.ResponseWriter, r *http.Request) {
	var req vehicleRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	input, fields := req.toInput()
	if fields != nil {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}

	vehicle, err := s.Store.UpdateVehicle(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"), input)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeVehicleNotFound, "Vehicle not found.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not save the vehicle.")
		return
	}
	WriteJSON(w, http.StatusOK, vehicle)
}

func (s *Server) handleDeleteVehicle(w http.ResponseWriter, r *http.Request) {
	err := s.Store.DeleteVehicle(r.Context(), UserIDFrom(r.Context()), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeVehicleNotFound, "Vehicle not found.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not remove the vehicle.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

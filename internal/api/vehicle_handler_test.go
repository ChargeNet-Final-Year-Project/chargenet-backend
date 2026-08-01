package api_test

import (
	"net/http"
	"testing"
	"time"
)

type vehicleDTO struct {
	ID              string  `json:"id"`
	UserID          string  `json:"userId"`
	Name            string  `json:"name"`
	LicensePlate    string  `json:"licensePlate"`
	ConnectorType   string  `json:"connectorType"`
	BatteryCapacity float64 `json:"batteryCapacity"`
	IsDefault       bool    `json:"isDefault"`
}

// authTokenFor registers a driver with a specific email (tests that need a
// second, isolated account).
func authTokenFor(t *testing.T, h http.Handler, email string) string {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Driver", "email": email, "password": "Secret123",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d (%s)", email, rec.Code, rec.Body)
	}
	return decode[authResp](t, rec).Token
}

func vehicleBody(name, plate, connector string, battery float64, isDefault bool) map[string]any {
	return map[string]any{
		"name":            name,
		"licensePlate":    plate,
		"connectorType":   connector,
		"batteryCapacity": battery,
		"isDefault":       isDefault,
	}
}

func createVehicle(t *testing.T, h http.Handler, token string, body map[string]any) vehicleDTO {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/api/v1/vehicles", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create vehicle = %d (%s)", rec.Code, rec.Body)
	}
	return decode[vehicleDTO](t, rec)
}

func listVehicles(t *testing.T, h http.Handler, token string) []vehicleDTO {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/vehicles", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list vehicles = %d (%s)", rec.Code, rec.Body)
	}
	return decode[[]vehicleDTO](t, rec)
}

func countDefaults(vs []vehicleDTO) int {
	n := 0
	for _, v := range vs {
		if v.IsDefault {
			n++
		}
	}
	return n
}

func TestVehicleCreateDefaults(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	if list := listVehicles(t, h, token); len(list) != 0 {
		t.Fatalf("new driver has %d vehicles, want 0", len(list))
	}

	v1 := createVehicle(t, h, token, vehicleBody("Perodua Myvi", "abc 123", "type2", 40, false))
	if !v1.IsDefault {
		t.Fatalf("first vehicle should be the default, got %+v", v1)
	}
	if v1.LicensePlate != "ABC 123" {
		t.Fatalf("licensePlate = %q, want ABC 123 (upper-cased)", v1.LicensePlate)
	}
	if v1.ConnectorType != "TYPE2" {
		t.Fatalf("connectorType = %q, want TYPE2", v1.ConnectorType)
	}

	v2 := createVehicle(t, h, token, vehicleBody("BYD Atto 3", "wxy 9876", "CCS2", 60, false))
	if v2.IsDefault {
		t.Fatalf("second vehicle must not take over the default flag")
	}

	list := listVehicles(t, h, token)
	if len(list) != 2 {
		t.Fatalf("vehicle count = %d, want 2", len(list))
	}
	if n := countDefaults(list); n != 1 {
		t.Fatalf("default count = %d, want 1", n)
	}
	if !list[0].IsDefault {
		t.Fatalf("default vehicle should be listed first: %+v", list)
	}
}

func TestVehicleIgnoresBodyIdentity(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	body := vehicleBody("Injected", "HACK 1", "CCS2", 50, false)
	body["id"] = "veh_injected"
	body["userId"] = "usr_injected"

	v := createVehicle(t, h, token, body)
	if v.ID == "veh_injected" || v.UserID == "usr_injected" {
		t.Fatalf("server honoured client-supplied identity: %+v", v)
	}
}

func TestVehicleUpdateFlipsDefault(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	v1 := createVehicle(t, h, token, vehicleBody("Car One", "AAA 1", "CCS2", 60, false))
	v2 := createVehicle(t, h, token, vehicleBody("Car Two", "BBB 2", "CCS2", 70, false))

	rec := doJSON(t, h, http.MethodPut, "/api/v1/vehicles/"+v2.ID, token,
		vehicleBody("Car Two Renamed", "bbb 2", "CCS2", 72, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body)
	}
	updated := decode[vehicleDTO](t, rec)
	if !updated.IsDefault || updated.Name != "Car Two Renamed" || updated.BatteryCapacity != 72 {
		t.Fatalf("updated vehicle = %+v", updated)
	}

	list := listVehicles(t, h, token)
	if n := countDefaults(list); n != 1 {
		t.Fatalf("default count = %d, want 1: %+v", n, list)
	}
	for _, v := range list {
		if v.ID == v1.ID && v.IsDefault {
			t.Fatalf("old default was not unset: %+v", list)
		}
		if v.ID == v2.ID && !v.IsDefault {
			t.Fatalf("new default was not persisted: %+v", list)
		}
	}
}

func TestVehicleDeletePromotesOldest(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	v1 := createVehicle(t, h, token, vehicleBody("Car One", "AAA 1", "CCS2", 60, false))
	time.Sleep(2 * time.Millisecond) // guarantee distinct created_at values
	v2 := createVehicle(t, h, token, vehicleBody("Car Two", "BBB 2", "CCS2", 65, false))
	time.Sleep(2 * time.Millisecond)
	createVehicle(t, h, token, vehicleBody("Car Three", "CCC 3", "CCS2", 70, false))

	rec := doJSON(t, h, http.MethodDelete, "/api/v1/vehicles/"+v1.ID, token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s), want 204", rec.Code, rec.Body)
	}

	list := listVehicles(t, h, token)
	if len(list) != 2 {
		t.Fatalf("vehicle count = %d, want 2", len(list))
	}
	if n := countDefaults(list); n != 1 {
		t.Fatalf("default count = %d, want 1: %+v", n, list)
	}
	if !list[0].IsDefault || list[0].ID != v2.ID {
		t.Fatalf("oldest remaining (%s) should be promoted, got %+v", v2.ID, list)
	}
}

func TestVehicleOwnershipIsolation(t *testing.T) {
	h, _ := newTestRouter(t)
	owner := authToken(t, h)
	other := authTokenFor(t, h, "intruder@example.com")

	mine := createVehicle(t, h, owner, vehicleBody("Mine", "OWN 1", "CCS2", 60, false))

	if list := listVehicles(t, h, other); len(list) != 0 {
		t.Fatalf("other driver sees %d vehicles, want 0", len(list))
	}

	rec := doJSON(t, h, http.MethodPut, "/api/v1/vehicles/"+mine.ID, other,
		vehicleBody("Stolen", "OWN 1", "CCS2", 60, true))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user update = %d, want 404", rec.Code)
	}
	if body := decode[errResp](t, rec); body.Error.Code != "VEHICLE_NOT_FOUND" {
		t.Fatalf("code = %q, want VEHICLE_NOT_FOUND", body.Error.Code)
	}

	rec = doJSON(t, h, http.MethodDelete, "/api/v1/vehicles/"+mine.ID, other, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete = %d, want 404", rec.Code)
	}

	// The owner's vehicle is untouched.
	if list := listVehicles(t, h, owner); len(list) != 1 || list[0].Name != "Mine" {
		t.Fatalf("owner's vehicle changed: %+v", list)
	}
}

func TestVehicleValidation(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodPost, "/api/v1/vehicles", token, map[string]any{
		"name": "", "licensePlate": "   ", "connectorType": "MAGIC", "batteryCapacity": 5,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	body := decode[errResp](t, rec)
	if body.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("code = %q, want VALIDATION_ERROR", body.Error.Code)
	}
	for _, field := range []string{"name", "licensePlate", "connectorType", "batteryCapacity"} {
		if _, ok := body.Error.Fields[field]; !ok {
			t.Fatalf("missing validation message for %q: %v", field, body.Error.Fields)
		}
	}
}

func TestVehicleUnknownIDIsNotFound(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodDelete, "/api/v1/vehicles/veh_missing", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", rec.Code)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/vehicles", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("list without token = %d, want 401", rec.Code)
	}
}

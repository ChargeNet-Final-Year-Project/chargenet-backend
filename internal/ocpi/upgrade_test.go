package ocpi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
)

func Test230AndLegacyPriceRepresentations(t *testing.T) {
	raw, err := json.Marshal(ocpi.Price{ExclVAT: 0})
	if err != nil || string(raw) != `{"before_taxes":0}` {
		t.Fatalf("required zero price %s %v", raw, err)
	}
	for _, value := range []string{`{"before_taxes":9.00}`, `{"excl_vat":9.00}`} {
		var price ocpi.Price
		if err = json.Unmarshal([]byte(value), &price); err != nil || price.ExclVAT != 9 {
			t.Fatalf("price %s %v", value, err)
		}
	}
	current := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocpi/emsp/2.3.0/sessions" {
			t.Errorf("translated path %s", r.URL.Path)
		}
		ocpi.OK(w, 200, map[string]any{"version": "2.3.0", "total_cost": ocpi.Price{ExclVAT: 9}, "tax_included": "N/A"})
	})
	legacy := ocpi.LegacyAdapter(current, "/ocpi/emsp/2.2.1", "/ocpi/emsp/2.3.0")
	response := httptest.NewRecorder()
	legacy.ServeHTTP(response, httptest.NewRequest("GET", "/ocpi/emsp/2.2.1/sessions", nil))
	if !strings.Contains(response.Body.String(), `"excl_vat":9`) || strings.Contains(response.Body.String(), "before_taxes") || strings.Contains(response.Body.String(), "tax_included") {
		t.Fatalf("legacy wire %s", response.Body)
	}
}

func Test230DiscoveryDoesNotAdvertiseOptionalPayments(t *testing.T) {
	h, _ := newServer(t)
	details := success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/", voltgoTokenC, nil))
	for _, endpoint := range details.Endpoints {
		if endpoint.Identifier == "payments" || endpoint.Identifier == "bookings" {
			t.Fatalf("unsupported module %s", endpoint.Identifier)
		}
	}
	legacy := success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, "/ocpi/emsp/2.2.1/", voltgoTokenC, nil))
	if legacy.Version != "2.2.1" {
		t.Fatalf("legacy negotiation %s", legacy.Version)
	}
	for _, endpoint := range legacy.Endpoints {
		if !strings.Contains(endpoint.URL, "/2.2.1/") {
			t.Fatal("legacy endpoint has wrong version")
		}
	}
	missing := do(t, h, "GET", ocpi.VersionURLPath+"/payments", voltgoTokenC, nil)
	if missing.Code != 404 {
		t.Fatalf("unsupported payments status %d", missing.Code)
	}
}

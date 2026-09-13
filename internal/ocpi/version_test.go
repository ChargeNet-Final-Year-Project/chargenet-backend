package ocpi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
)

func TestPriceUsesBeforeTaxes(t *testing.T) {
	raw, err := json.Marshal(ocpi.Price{BeforeTaxes: 0})
	if err != nil || string(raw) != `{"before_taxes":0}` {
		t.Fatalf("price = %s, error = %v", raw, err)
	}
	var price ocpi.Price
	if err := json.Unmarshal([]byte(`{"before_taxes":9}`), &price); err != nil || price.BeforeTaxes != 9 {
		t.Fatalf("price = %+v, error = %v", price, err)
	}
	if err := json.Unmarshal([]byte(`{"other":9}`), &price); err == nil {
		t.Fatal("price without before_taxes was accepted")
	}
}

func TestVersionDetailsAdvertiseImplementedModules(t *testing.T) {
	h, _ := newServer(t)
	details := success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/", voltgoTokenC, nil))
	if details.Version != ocpi.VersionNumber {
		t.Fatalf("version = %q", details.Version)
	}
	for _, endpoint := range details.Endpoints {
		if endpoint.Identifier == "payments" || endpoint.Identifier == "bookings" {
			t.Fatalf("unsupported module %s", endpoint.Identifier)
		}
	}
	if rec := do(t, h, http.MethodGet, ocpi.VersionURLPath+"/payments", voltgoTokenC, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unsupported module status = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/ocpi/emsp/2.4.0/", voltgoTokenC, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unsupported version status = %d", rec.Code)
	}
}

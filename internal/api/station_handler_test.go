package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// authToken registers a throwaway user and returns a valid bearer token.
func authToken(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Tester", "email": "tester@example.com", "password": "Secret123",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register for token: status %d (%s)", rec.Code, rec.Body)
	}
	return decode[authResp](t, rec).Token
}

func TestListStationsSortedByDistance(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?lat=3.14&lng=101.69", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	stations := decode[[]struct {
		Name        string   `json:"name"`
		CpoName     string   `json:"cpoName"`
		DistanceKm  *float64 `json:"distanceKm"`
		Connectors  []any    `json:"connectors"`
		CpoColorHex string   `json:"cpoColorHex"`
	}](t, rec)

	if len(stations) < 14 {
		t.Fatalf("station count = %d, want ≥14", len(stations))
	}
	for i, s := range stations {
		if s.DistanceKm == nil {
			t.Fatalf("station %d (%s) missing distanceKm", i, s.Name)
		}
		if len(s.Connectors) == 0 {
			t.Fatalf("station %s has no connectors", s.Name)
		}
		if s.CpoColorHex == "" {
			t.Fatalf("station %s missing cpoColorHex", s.Name)
		}
		if i > 0 && *stations[i-1].DistanceKm > *s.DistanceKm {
			t.Fatalf("stations not sorted ascending by distance at %d", i)
		}
	}
}

func TestListStationsConnectorTypeFilter(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?connectorType=TYPE2", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	stations := decode[[]struct {
		Connectors []struct {
			Standard string `json:"standard"`
		} `json:"connectors"`
	}](t, rec)
	if len(stations) == 0 {
		t.Fatal("expected some TYPE2 stations")
	}
	for _, s := range stations {
		found := false
		for _, connector := range s.Connectors {
			if connector.Standard == "TYPE2" {
				found = true
			}
		}
		if !found {
			t.Fatal("TYPE2 filter returned a station without a TYPE2 connector")
		}
	}
}

func TestListStationsAvailableAndMinPowerFilter(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?availableOnly=true&minPower=150", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	stations := decode[[]struct {
		CpoName string `json:"cpoName"`
	}](t, rec)
	if len(stations) == 0 {
		t.Fatal("expected VoltGo stations with ≥150 kW available")
	}
	for _, s := range stations {
		if s.CpoName != "VoltGo" {
			t.Fatalf("filter returned %s, want only VoltGo", s.CpoName)
		}
	}
}

func TestListStationsQueryFilter(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?query=mid%20valley", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	stations := decode[[]struct {
		Name string `json:"name"`
	}](t, rec)
	if len(stations) != 1 || stations[0].Name != "ChargeSync Mid Valley" {
		t.Fatalf("query returned %+v, want [ChargeSync Mid Valley]", stations)
	}
}

func TestGetEVSE(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/evse/vlt-sgb-01", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	body := decode[struct {
		Station struct {
			Name string `json:"name"`
		} `json:"station"`
		Connector struct {
			EVSEUID    string  `json:"evseUid"`
			Standard   string  `json:"standard"`
			MaxPowerKW float64 `json:"maxPowerKw"`
		} `json:"connector"`
	}](t, rec)
	if body.Connector.EVSEUID != "VLT-SGB-01" || body.Connector.Standard != "CCS2" {
		t.Fatalf("unexpected connector %+v", body.Connector)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/evse/NOPE-XXX-99", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown evse status = %d, want 404", rec.Code)
	}
}

// suspendCpo suspends a partner for the duration of the test and puts the row
// back exactly as it was afterwards. The seed partners are shared with every
// other test in the package — testutil resets the user tables, not the network
// ones — so a partner left SUSPENDED would hide its stations from all of them.
func suspendCpo(t *testing.T, st *store.Store, cpoID string) {
	t.Helper()
	ctx := context.Background()

	var status, handshake string
	var tokA, tokB, tokC *string
	err := st.DB.QueryRowContext(ctx,
		`SELECT status, handshake_status, token_a, token_b, token_c
		   FROM cpo_partners WHERE id = $1`, cpoID).
		Scan(&status, &handshake, &tokA, &tokB, &tokC)
	if err != nil {
		t.Fatalf("snapshot partner %s: %v", cpoID, err)
	}
	t.Cleanup(func() {
		if _, err := st.DB.ExecContext(context.Background(),
			`UPDATE cpo_partners
			    SET status = $2, handshake_status = $3, token_a = $4, token_b = $5, token_c = $6
			  WHERE id = $1`, cpoID, status, handshake, tokA, tokB, tokC); err != nil {
			t.Errorf("restore partner %s: %v", cpoID, err)
		}
	})

	if err := st.SuspendCPO(ctx, cpoID); err != nil {
		t.Fatalf("SuspendCPO(%s): %v", cpoID, err)
	}
}

// TestSuspendedCpoHiddenFromDiscovery is the discovery half of the OCPI gateway's enforcement
// (the sessions package covers the start half): a suspended partner drops out of
// the station list, and a link a driver already holds is refused at the station
// and at the charger — as if both were simply gone.
func TestSuspendedCpoHiddenFromDiscovery(t *testing.T) {
	h, st := newTestRouter(t)
	token := authToken(t, h)
	ctx := context.Background()

	// A seeded VoltGo bay anchors the partner that is about to be suspended.
	station, conn, err := st.GetEVSE(ctx, "VLT-SGB-01")
	if err != nil {
		t.Fatalf("GetEVSE: %v", err)
	}
	suspendCpo(t, st, station.CpoID)

	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list stations: status %d (%s)", rec.Code, rec.Body)
	}
	for _, s := range decode[[]struct {
		CpoName string `json:"cpoName"`
	}](t, rec) {
		if s.CpoName == "VoltGo" {
			t.Fatal("a suspended partner is still listed")
		}
	}

	if rec := doJSON(t, h, http.MethodGet, "/api/v1/stations/"+station.ID, token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("suspended station = %d (%s), want 404", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodGet, "/api/v1/evse/"+conn.EVSEUID, token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("suspended EVSE = %d (%s), want 404", rec.Code, rec.Body)
	}
}

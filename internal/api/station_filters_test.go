package api_test

import (
	"net/http"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/store"
)

func TestStationRegionAndPowerFilters(t *testing.T) {
	h, _ := newTestRouter(t)
	token := authToken(t, h)
	for _, tc := range []struct {
		query string
		count int
	}{
		{"country=MY&region=Penang", 11},
		{"country=SG", 6},
		{"country=TH", 7},
		{"country=MY&region=Penang&powerType=AC&minPower=10&maxPower=22", 11},
		// An AC connector must itself meet the power condition: a DC sibling
		// cannot make its station match this conjunction.
		{"country=MY&region=Penang&powerType=AC&minPower=50", 0},
		{"country=SG&region=Central", 3},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?"+tc.query, token, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			rows := decode[[]store.Station](t, rec)
			if len(rows) != tc.count {
				t.Fatalf("got %d stations, want %d", len(rows), tc.count)
			}
		})
	}
	rec := doJSON(t, h, http.MethodGet, "/api/v1/stations/regions", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	regions := decode[[]store.StationRegion](t, rec)
	penang := false
	for _, r := range regions {
		if r.CountryCode == "MY" && r.Name == "Penang" {
			penang = true
			if r.StationCount != 11 || r.South < 5.2 || r.North > 5.5 || r.West < 100.2 || r.East > 100.5 {
				t.Fatalf("wrong Penang coverage: %+v", r)
			}
		}
	}
	if !penang {
		t.Fatal("Penang missing from region options")
	}
	for _, query := range []string{"minPower=NaN", "maxPower=Infinity", "minPower=-1", "minPower=100&maxPower=22", "powerType=HYBRID", "country=XX", "region=Penang"} {
		if rec := doJSON(t, h, http.MethodGet, "/api/v1/stations?"+query, token, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid filter %s: %d", query, rec.Code)
		}
	}
}

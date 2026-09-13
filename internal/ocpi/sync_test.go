package ocpi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// The pull sync of the OCPI gateway: ChargeNet re-reads each active partner's locations
// and tariffs, so a push that was lost — or one a partner never makes — cannot
// leave the network stale. These tests stand a partner up on httptest and drive
// the sync by hand.

// newPartner serves the two modules the pull reads, authenticating every call
// with the Token B ChargeNet must present. A module handler is
// supplied per test, so the same helper covers the happy path and the modules a
// partner may not serve.
func newPartner(t *testing.T, token string, locations, tariffs http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mount := func(path string, h http.HandlerFunc) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Token "+base64.StdEncoding.EncodeToString([]byte(token)) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		})
	}
	mount("/2.3.0/locations", locations)
	mount("/2.3.0/tariffs", tariffs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// listOf answers a module with an OCPI success envelope carrying payload.
func listOf(payload any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 1000, "data": payload})
	}
}

// notServed is a module a partner does not implement: an OCPI 3001 failure.
func notServed(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status_code": 3001, "status_message": "Not supported", "data": nil,
	})
}

// syncTestPartner registers the throwaway OCPI partner the pull tests read from.
//
// The sync writes into the shared network tables — stations, connectors, tariffs
// and cpo_partners — which testutil deliberately leaves alone because they hold
// the seed. A test therefore brings its own partner and takes its rows away
// again: a leftover station would otherwise become the next test's seed data,
// and the next run's duplicate key.
func syncTestPartner(t *testing.T, st *store.Store, baseURL string) *store.CpoAccount {
	t.Helper()
	const (
		id    = "cpo_synctest"
		slug  = "synctest"
		token = "sync-token-b"
	)
	purgeSyncPartner(t, st, id)
	if _, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO cpo_partners (id, name, slug, country_code, party_id, base_url,
		                           color_hex, contact_email, token_b, status)
		 VALUES ($1, 'SyncTest', $2, 'MY', 'SYT', $3, '#6B7280', 'ops@synctest.my', $4, 'ACTIVE')`,
		id, slug, baseURL, token); err != nil {
		t.Fatalf("seed the sync partner: %v", err)
	}
	t.Cleanup(func() { purgeSyncPartner(t, st, id) })
	return &store.CpoAccount{ID: id, Slug: slug, CountryCode: "MY", PartyID: "SYT", BaseURL: baseURL, TokenB: token}
}

// purgeSyncPartner removes a test partner and everything a pull could have
// written for it, handing the shared tables on exactly as they were found.
func purgeSyncPartner(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM connectors WHERE station_id IN (SELECT id FROM stations WHERE cpo_id = $1)`,
		`DELETE FROM stations WHERE cpo_id = $1`,
		`DELETE FROM tariffs WHERE cpo_id = $1`,
		`DELETE FROM cpo_partners WHERE id = $1`,
	} {
		if _, err := st.DB.ExecContext(ctx, q, id); err != nil {
			t.Fatalf("purge sync partner %s: %v", id, err)
		}
	}
}

// pullOnce builds a syncer whose periodic loop is off, so a test drives the pull
// through SyncOne/SyncAll directly.
func pullOnce(st *store.Store, srv *httptest.Server) *ocpi.Syncer {
	return ocpi.NewSyncer(st,
		ocpi.WithSyncClient(ocpi.NewClient(ocpi.WithHTTPClient(srv.Client()))),
		ocpi.WithSyncInterval(0))
}

// syncLocation is one location a partner publishes, with a single CCS2 bay.
func syncLocation() ocpi.Location {
	return ocpi.Location{
		CountryCode: "MY",
		PartyID:     "SYT",
		ID:          "SYN-9",
		Name:        "SyncTest Hub",
		Address:     "Jalan Sync 1",
		City:        "Shah Alam",
		Coordinates: ocpi.GeoLocation{Latitude: "3.0738", Longitude: "101.5183"},
		EVSEs: []ocpi.EVSE{{
			UID:    "SYN-9-A",
			Status: ocpi.EVSEAvailable,
			Connectors: []ocpi.Connector{{
				ID:               "1",
				Standard:         "IEC_62196_T2_COMBO",
				Format:           "CABLE",
				PowerType:        "DC",
				MaxElectricPower: 150000,
				TariffIDs:        []string{"trf_sync"},
			}},
		}},
	}
}

// syncTariff is the price the location above references.
func syncTariff() ocpi.Tariff {
	return ocpi.Tariff{
		CountryCode: "MY", PartyID: "SYT", ID: "trf_sync", Currency: "MYR",
		Elements: []ocpi.TariffElement{{
			PriceComponents: []ocpi.PriceComponent{{Type: "ENERGY", Price: 1.55, StepSize: 1}},
		}},
	}
}

// TestSyncOnePullsLocationsAndTariffs is the core of the OCPI gateway: one pull stores the
// partner's location and its price, so a location that only ever exists in the
// CPO's `GET locations` still reaches the driver app — priced, not at zero.
func TestSyncOnePullsLocationsAndTariffs(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	srv := newPartner(t, "sync-token-b", listOf([]ocpi.Location{syncLocation()}), listOf([]ocpi.Tariff{syncTariff()}))
	partner := syncTestPartner(t, st, srv.URL)
	syncer := pullOnce(st, srv)

	if err := syncer.SyncOne(ctx, partner); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}

	loc, err := st.GetOCPILocation(ctx, partner.ID, "SYN-9")
	if err != nil {
		t.Fatalf("the pulled location was not stored: %v", err)
	}
	if loc.Name != "SyncTest Hub" || loc.City != "Shah Alam" {
		t.Fatalf("location = %+v, want the pulled name and city", loc)
	}
	if len(loc.Connectors) != 1 {
		t.Fatalf("connectors = %d, want 1", len(loc.Connectors))
	}
	c := loc.Connectors[0]
	if c.EVSEUID != "SYN-9-A" || c.Standard != "CCS2" || c.Status != "AVAILABLE" {
		t.Fatalf("connector = %+v, want the mapped CCS2/AVAILABLE bay", c)
	}
	if c.MaxPowerKW != 150 {
		t.Fatalf("maxPowerKW = %v, want 150", c.MaxPowerKW)
	}
	// The tariff push reprices the connector the location already created.
	if c.TariffID != "trf_sync" || c.TariffPerKWh != 1.55 {
		t.Fatalf("connector tariff = %q @ %v, want trf_sync @ 1.55", c.TariffID, c.TariffPerKWh)
	}
}

// TestSyncWithoutTariffsModuleStillStoresLocations covers the OCPI gateway's "GET tariffs
// (optional)": a partner that does not serve the module still gets its locations
// reconciled, and the bay keeps the zero price the location alone implies.
func TestSyncWithoutTariffsModuleStillStoresLocations(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	srv := newPartner(t, "sync-token-b", listOf([]ocpi.Location{syncLocation()}), notServed)
	partner := syncTestPartner(t, st, srv.URL)
	syncer := pullOnce(st, srv)

	if err := syncer.SyncOne(ctx, partner); err != nil {
		t.Fatalf("a missing tariffs module must not fail the sync: %v", err)
	}
	loc, err := st.GetOCPILocation(ctx, partner.ID, "SYN-9")
	if err != nil {
		t.Fatalf("the pulled location was not stored: %v", err)
	}
	if got := loc.Connectors[0].TariffPerKWh; got != 0 {
		t.Fatalf("tariffPerKwh = %v, want 0 with no tariff module", got)
	}
}

// TestSyncOneReportsAnUnreachablePartner ensures a 5xx reaches the periodic loop.
func TestSyncOneReportsAnUnreachablePartner(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	down := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	srv := newPartner(t, "sync-token-b", down, down)
	partner := syncTestPartner(t, st, srv.URL)
	syncer := pullOnce(st, srv)

	if err := syncer.SyncOne(ctx, partner); err == nil {
		t.Fatal("SyncOne accepted a 503 from the partner's locations module")
	}
}

// TestSyncSkipsMalformedLocation: one location the receiver would refuse is
// skipped without abandoning the rest of the pull, mirroring the receiver's own
// refusal of that document.
func TestSyncSkipsMalformedLocation(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	bad := syncLocation()
	bad.ID = "SYN-BAD"
	bad.City = "" // no city: the receiver refuses this location

	srv := newPartner(t, "sync-token-b", listOf([]ocpi.Location{bad, syncLocation()}), listOf([]ocpi.Tariff{syncTariff()}))
	partner := syncTestPartner(t, st, srv.URL)
	syncer := pullOnce(st, srv)

	if err := syncer.SyncOne(ctx, partner); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if _, err := st.GetOCPILocation(ctx, partner.ID, "SYN-9"); err != nil {
		t.Fatalf("the good location was not stored alongside the bad one: %v", err)
	}
	if _, err := st.GetOCPILocation(ctx, partner.ID, "SYN-BAD"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the malformed location was stored (err = %v), want it skipped", err)
	}
}

// muteSeedPartners points every partner other than keep at an unroutable
// address for the duration of a test, restoring them on cleanup.
//
// The seed's partners point at http://localhost:8080/emulator/<slug>/ocpi, so a
// developer running the stack locally would have SyncAll reach a live server:
// the emulator republishes ChargeNet's own network and the receiver's tariff
// upsert reprices the seeded connectors, rewriting the shared seed for every
// test that runs afterwards (and for the dev database). Nothing listens on
// 127.0.0.1:1, so the pull fails harmlessly — the same way it does in CI, where
// no emulator is up — while SyncAll still walks every active partner.
func muteSeedPartners(t *testing.T, st *store.Store, keep string) {
	t.Helper()
	ctx := context.Background()

	type partner struct{ id, baseURL string }
	saved := []partner{}
	rows, err := st.DB.QueryContext(ctx,
		`SELECT id, coalesce(base_url, '') FROM cpo_partners WHERE id <> $1 ORDER BY id`, keep)
	if err != nil {
		t.Fatalf("read partners to mute: %v", err)
	}
	for rows.Next() {
		var p partner
		if err := rows.Scan(&p.id, &p.baseURL); err != nil {
			rows.Close()
			t.Fatalf("scan partner: %v", err)
		}
		saved = append(saved, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read partners to mute: %v", err)
	}

	for _, p := range saved {
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE cpo_partners SET base_url = 'http://127.0.0.1:1/ocpi' WHERE id = $1`, p.id); err != nil {
			t.Fatalf("mute partner %s: %v", p.id, err)
		}
	}
	t.Cleanup(func() {
		for _, p := range saved {
			if _, err := st.DB.ExecContext(context.Background(),
				`UPDATE cpo_partners SET base_url = $2 WHERE id = $1`, p.id, p.baseURL); err != nil {
				t.Errorf("restore partner %s: %v", p.id, err)
			}
		}
	})
}

// TestSyncAllStoresEveryReachablePartner: SyncAll walks the active partners and
// reconciles each, and one partner's failure does not stop the others. The
// seeded partners are muted onto a dead address first, so they fail harmlessly
// whether or not a ChargeNet server is running on this machine, while the
// reachable partner we add is still pulled.
func TestSyncAllStoresEveryReachablePartner(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	srv := newPartner(t, "sync-token-b", listOf([]ocpi.Location{syncLocation()}), listOf([]ocpi.Tariff{syncTariff()}))
	partner := syncTestPartner(t, st, srv.URL)
	syncer := pullOnce(st, srv)
	muteSeedPartners(t, st, partner.ID)

	// SyncAll may report the unreachable seeded partners; it must still have
	// reconciled the one that is up.
	_ = syncer.SyncAll(ctx)

	loc, err := st.GetOCPILocation(ctx, partner.ID, "SYN-9")
	if err != nil {
		t.Fatalf("the reachable partner was skipped: %v", err)
	}
	if len(loc.Connectors) != 1 || loc.Connectors[0].TariffPerKWh != 1.55 {
		t.Fatalf("location = %+v, want one bay priced 1.55", loc)
	}
}

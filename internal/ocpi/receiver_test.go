package ocpi_test

// Receiver integration tests use the token middleware, receiver and database.
// The telemetry and command sinks are fakes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// recordingSink is the fake orchestrator: it keeps every event, so a test can
// assert what a driver's stream would have shown.
type recordingSink struct {
	mu   sync.Mutex
	seen []gateway.Telemetry
}

func (s *recordingSink) Publish(ev gateway.Telemetry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, ev)
}

// events returns a copy of everything published so far.
func (s *recordingSink) events() []gateway.Telemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gateway.Telemetry(nil), s.seen...)
}

// only fails the test unless exactly one event was published, and returns it:
// the assertions below want "and nothing else happened" checked too.
func (s *recordingSink) only(t *testing.T) gateway.Telemetry {
	t.Helper()
	events := s.events()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1: %+v", len(events), events)
	}
	return events[0]
}

// recordedCommand is one command result a CPO posted back.
type recordedCommand struct {
	Command string
	UID     string
	Result  ocpi.CommandResponse
}

// recordingCommands is the fake OCPI gateway the commands module hands an
// asynchronous answer to.
type recordingCommands struct {
	mu  sync.Mutex
	got []recordedCommand
}

func (c *recordingCommands) CommandResult(command, uid string, res ocpi.CommandResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, recordedCommand{Command: command, UID: uid, Result: res})
}

func (c *recordingCommands) results() []recordedCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedCommand(nil), c.got...)
}

// newReceiverServer wires the OCPI receivers receivers against the shared test database
// with both sinks recording.
func newReceiverServer(t *testing.T) (http.Handler, *store.Store, *recordingSink, *recordingCommands) {
	t.Helper()
	st := testutil.NewTestStore(t)
	srv := ocpi.New(config.Config{PublicBaseURL: publicBaseURL, PlatformFeePct: 5}, st)
	sink, commands := &recordingSink{}, &recordingCommands{}
	srv.Sink, srv.Commands = sink, commands
	return srv.Handler(), st, sink, commands
}

// envelope decodes any OCPI envelope, success or failure, so a test can look at
// the status code a receiver answered with and at the message it gave.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) (json.RawMessage, int, string) {
	t.Helper()
	var env struct {
		Data          json.RawMessage `json:"data"`
		StatusCode    int             `json:"status_code"`
		StatusMessage string          `json:"status_message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	return env.Data, env.StatusCode, env.StatusMessage
}

// queryText, queryFloat and queryInt read one column of one row straight out of
// the test database, so a test asserts what a push wrote without going through a
// read path that is itself under test.
func queryText(t *testing.T, st *store.Store, query string, args ...any) string {
	t.Helper()
	var out string
	if err := st.DB.QueryRowContext(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

func queryFloat(t *testing.T, st *store.Store, query string, args ...any) float64 {
	t.Helper()
	var out float64
	if err := st.DB.QueryRowContext(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

func queryInt(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var out int
	if err := st.DB.QueryRowContext(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

// closeEnough compares a stored decimal with the value a push asked for.
func closeEnough(t *testing.T, what string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 0.001 || diff < -0.001 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// chargeFixture is the ChargeNet side of a CPO push about one running charge:
// the partner, the driver and their wallet, the station and connector the charge
// runs on, and the live session itself.
type chargeFixture struct {
	CPOID        string
	TokenC       string
	UserID       string
	SessionID    string
	StationID    string
	ConnectorID  string
	EVSEUID      string
	TariffPerKWh float64
	StartTime    time.Time
}

// newFixtureCharge inserts the rows a session or CDR push is about. testutil
// empties the driver tables between tests but deliberately leaves stations and
// connectors, so the fixture removes its own rows again — the CDR and the
// session first (they reference the station), then the station, then the driver,
// whose wallet cascades. The partner is registered by [newFixtureCPO] and is
// cleaned up after this, so these rows are gone by then.
func newFixtureCharge(t *testing.T, st *store.Store, partyID string) chargeFixture {
	t.Helper()
	c := chargeFixture{
		CPOID:        newFixtureCPO(t, st, partyID, "", partyID+"-token-c"),
		TokenC:       partyID + "-token-c",
		UserID:       "usr_" + partyID,
		SessionID:    "csn_" + partyID,
		StationID:    "stn_" + partyID,
		ConnectorID:  "con_" + partyID,
		EVSEUID:      "MY*" + partyID + "*E1",
		TariffPerKWh: 1.20,
		StartTime:    time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second),
	}

	ctx := context.Background()
	exec := func(what, query string, args ...any) {
		t.Helper()
		if _, err := st.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("insert fixture %s: %v", what, err)
		}
	}
	exec("user", `INSERT INTO users (id, name, email, password_hash)
		 VALUES ($1, 'OCPI Driver', $2, 'x')`, c.UserID, c.UserID+"@fixture.test")
	exec("wallet", `INSERT INTO wallets (id, user_id, balance) VALUES ($1, $2, 50.00)`,
		"wlt_"+partyID, c.UserID)
	exec("station", `INSERT INTO stations (id, cpo_id, ocpi_location_id, name, address, city,
		                        latitude, longitude, total_bays, available_bays)
		 VALUES ($1, $2, $3, 'Fixture Station', '1 Jalan Fixture', 'Kuala Lumpur',
		         3.100000, 101.600000, 1, 0)`, c.StationID, c.CPOID, partyID+"-LOC")
	exec("connector", `INSERT INTO connectors (id, station_id, evse_uid, ocpi_connector_id,
		                           standard, format, power_type, max_power_kw, tariff_per_kwh, status)
		 VALUES ($1, $2, $3, '1', 'CCS2', 'CABLE', 'DC', 60.00, $4, 'CHARGING')`,
		c.ConnectorID, c.StationID, c.EVSEUID, c.TariffPerKWh)
	exec("session", `INSERT INTO charging_sessions (id, user_id, cpo_id, station_id, connector_id,
		                                 evse_uid, status, start_time, battery_soc, tariff_per_kwh)
		 VALUES ($1, $2, $3, $4, $5, $6, 'CHARGING', $7, 35.00, $8)`,
		c.SessionID, c.UserID, c.CPOID, c.StationID, c.ConnectorID, c.EVSEUID, c.StartTime, c.TariffPerKWh)

	t.Cleanup(func() {
		bg := context.Background()
		clean := func(query string, args ...any) {
			if _, err := st.DB.ExecContext(bg, query, args...); err != nil {
				t.Logf("clean fixture charge: %v", err)
			}
		}
		clean(`DELETE FROM wallet_transactions WHERE session_id IN (SELECT id FROM charging_sessions WHERE cpo_id=$1)`, c.CPOID)
		clean(`DELETE FROM cdrs WHERE cpo_id = $1`, c.CPOID)
		clean(`DELETE FROM charging_sessions WHERE cpo_id = $1`, c.CPOID)
		clean(`DELETE FROM stations WHERE id = $1`, c.StationID)
		clean(`DELETE FROM users WHERE id = $1`, c.UserID)
	})
	return c
}

// TestLocationPushStoresStationAndConnectors covers the OCPI-to-app vocabulary mapping.
func TestLocationPushStoresStationAndConnectors(t *testing.T) {
	h, st, _, _ := newReceiverServer(t)
	cpoID := newFixtureCPO(t, st, "T30", "", "loc-token-c")
	ctx := context.Background()
	t.Cleanup(func() {
		// Stations are left alone between tests, so the pushed one goes too (its
		// connectors cascade with it, and no session points at it).
		if _, err := st.DB.ExecContext(ctx, `DELETE FROM stations WHERE cpo_id = $1`, cpoID); err != nil {
			t.Logf("clean pushed location: %v", err)
		}
	})

	body := ocpi.Location{
		CountryCode: "MY",
		PartyID:     "T30",
		ID:          "T30-LOC",
		Name:        "Bukit Bintang Hub",
		Address:     "12 Jalan Bukit Bintang",
		City:        "Kuala Lumpur",
		Coordinates: ocpi.GeoLocation{Latitude: "3.146300", Longitude: "101.710200"},
		EVSEs: []ocpi.EVSE{
			{
				UID:    "MY*T30*E1",
				Status: ocpi.EVSEAvailable,
				Connectors: []ocpi.Connector{{
					ID:               "1",
					Standard:         "IEC_62196_T2",
					Format:           "SOCKET",
					PowerType:        "AC",
					MaxElectricPower: 22000,
					TariffIDs:        []string{"TAR-T30"},
				}},
			},
			{
				UID:    "MY*T30*E2",
				Status: ocpi.EVSECharging,
				Connectors: []ocpi.Connector{
					{ID: "1", Standard: "IEC_62196_T2_COMBO", Format: "CABLE", PowerType: "DC",
						MaxVoltage: 400, MaxAmperage: 125, TariffIDs: []string{"TAR-T30"}},
					{ID: "2", Standard: "CHADEMO", Format: "CABLE", PowerType: "DC"},
				},
			},
		},
	}
	pushLocation := func(b ocpi.Location) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPut, ocpi.VersionURLPath+"/locations/MY/T30/"+b.ID, "loc-token-c", b)
	}

	echoed := success[ocpi.Location](t, pushLocation(body))
	if echoed.ID != "T30-LOC" || len(echoed.EVSEs) != 2 {
		t.Fatalf("the push was not echoed back: %+v", echoed)
	}

	loc, err := st.GetOCPILocation(ctx, cpoID, "T30-LOC")
	if err != nil {
		t.Fatalf("read the pushed location back: %v", err)
	}
	if loc.Name != "Bukit Bintang Hub" || loc.Address != "12 Jalan Bukit Bintang" || loc.City != "Kuala Lumpur" {
		t.Fatalf("station = %+v", loc)
	}
	closeEnough(t, "latitude", loc.Latitude, 3.146300)
	closeEnough(t, "longitude", loc.Longitude, 101.710200)

	// The bays are counted, not pushed: three connectors, one of them available.
	if loc.TotalBays != 3 || loc.Available != 1 {
		t.Fatalf("bays = %d total / %d available, want 3 / 1", loc.TotalBays, loc.Available)
	}

	stored := map[string]store.OCPIConnectorRead{}
	for _, c := range loc.Connectors {
		stored[c.EVSEUID] = c
	}
	if len(stored) != 3 {
		t.Fatalf("stored %d connectors, want 3: %+v", len(stored), loc.Connectors)
	}
	// The first socket of an EVSE keeps the EVSE uid; a second one is suffixed
	// with its OCPI connector id, so both stay scannable rows.
	ac, ok := stored["MY*T30*E1"]
	if !ok {
		t.Fatalf("the single-socket EVSE is missing: %+v", loc.Connectors)
	}
	if ac.Standard != "TYPE2" || ac.Format != "SOCKET" || ac.PowerType != "AC" || ac.OCPIConnectorID != "1" {
		t.Fatalf("the Type 2 bay was translated wrongly: %+v", ac)
	}
	closeEnough(t, "the AC bay's power", ac.MaxPowerKW, 22) // 22 000 W
	if ac.Status != "AVAILABLE" || ac.TariffID != "TAR-T30" {
		t.Fatalf("the Type 2 bay = %+v", ac)
	}

	dc, ok := stored["MY*T30*E2"]
	if !ok {
		t.Fatalf("the first socket of the two-socket EVSE is missing: %+v", loc.Connectors)
	}
	if dc.Standard != "CCS2" || dc.PowerType != "DC" || dc.Status != "CHARGING" {
		t.Fatalf("the CCS2 bay = %+v", dc)
	}
	closeEnough(t, "the CCS2 bay's power", dc.MaxPowerKW, 50) // 400 V × 125 A

	cha, ok := stored["MY*T30*E2:2"]
	if !ok {
		t.Fatalf("the second socket was not suffixed: %+v", loc.Connectors)
	}
	if cha.Standard != "CHADEMO" || cha.OCPIConnectorID != "2" || cha.TariffID != "" {
		t.Fatalf("the CHAdeMO bay = %+v", cha)
	}
	closeEnough(t, "the CHAdeMO bay's power", cha.MaxPowerKW, 0)

	// Nothing prices these bays yet: the tariff push is what does.
	closeEnough(t, "the stored price of TAR-T30", ac.TariffPerKWh, 0)

	t.Run("a location with no address is refused", func(t *testing.T) {
		broken := body
		broken.ID, broken.Address = "T30-NOADDR", ""
		msg := failure(t, pushLocation(broken), http.StatusBadRequest, ocpi.StatusClientError)
		if !strings.Contains(msg, "address") {
			t.Fatalf("status message = %q", msg)
		}
		if _, err := st.GetOCPILocation(ctx, cpoID, "T30-NOADDR"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused location was stored anyway: %v", err)
		}
	})

	t.Run("a push for another party is refused", func(t *testing.T) {
		msg := failure(t,
			do(t, h, http.MethodPut, ocpi.VersionURLPath+"/locations/MY/XX9/T30-LOC", "loc-token-c", body),
			http.StatusForbidden, ocpi.StatusUnauthorized)
		if !strings.Contains(msg, "party") {
			t.Fatalf("status message = %q", msg)
		}
	})

	t.Run("the locations module serves no reads", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/locations/MY/T30/T30-LOC", "loc-token-c", nil),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
	})

	// A re-push replaces the location: an EVSE the CPO stopped reporting is
	// gone, while the station the driver already knows keeps its identity.
	stationID := loc.ID
	body.EVSEs = body.EVSEs[:1]
	success[ocpi.Location](t, pushLocation(body))
	loc, err = st.GetOCPILocation(ctx, cpoID, "T30-LOC")
	if err != nil {
		t.Fatalf("read the re-pushed location back: %v", err)
	}
	if loc.ID != stationID {
		t.Fatalf("the re-push created a second station: %s, was %s", loc.ID, stationID)
	}
	if loc.TotalBays != 1 || loc.Available != 1 || len(loc.Connectors) != 1 {
		t.Fatalf("the re-push did not replace the location: %+v", loc)
	}
}

// TestLocationPatchUpdatesEvseStatuses covers the PATCH half of OCPI receivers:
// the two shapes the spec allows, the statuses ChargeNet can represent, and the
// promise that a patch lands whole or not at all.
func TestLocationPatchUpdatesEvseStatuses(t *testing.T) {
	h, st, _, _ := newReceiverServer(t)
	cpoID := newFixtureCPO(t, st, "T31", "", "patch-token-c")
	ctx := context.Background()
	t.Cleanup(func() {
		if _, err := st.DB.ExecContext(ctx, `DELETE FROM stations WHERE cpo_id = $1`, cpoID); err != nil {
			t.Logf("clean pushed location: %v", err)
		}
	})

	body := ocpi.Location{
		CountryCode: "MY", PartyID: "T31", ID: "T31-LOC",
		Name: "Patch Test Hub", Address: "3 Jalan Patch", City: "Kuala Lumpur",
		Coordinates: ocpi.GeoLocation{Latitude: "3.100000", Longitude: "101.600000"},
		EVSEs: []ocpi.EVSE{
			{UID: "MY*T31*E1", Status: ocpi.EVSEAvailable, Connectors: []ocpi.Connector{
				{ID: "1", Standard: "IEC_62196_T2", Format: "CABLE", PowerType: "AC", MaxElectricPower: 22000},
			}},
			{UID: "MY*T31*E2", Status: ocpi.EVSECharging, Connectors: []ocpi.Connector{
				{ID: "1", Standard: "IEC_62196_T2_COMBO", Format: "CABLE", PowerType: "DC", MaxElectricPower: 150000},
				{ID: "2", Standard: "IEC_62196_T2", Format: "CABLE", PowerType: "AC", MaxElectricPower: 22000},
			}},
		},
	}
	success[ocpi.Location](t,
		do(t, h, http.MethodPut, ocpi.VersionURLPath+"/locations/MY/T31/T31-LOC", "patch-token-c", body))
	loc, err := st.GetOCPILocation(ctx, cpoID, "T31-LOC")
	if err != nil {
		t.Fatalf("read the pushed location back: %v", err)
	}
	stationID := loc.ID

	statusOf := func(evseUID string) string {
		return queryText(t, st, `SELECT status FROM connectors WHERE evse_uid = $1`, evseUID)
	}
	availableBays := func() int {
		return queryInt(t, st, `SELECT available_bays FROM stations WHERE id = $1`, stationID)
	}
	patch := func(sub string, b any) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPatch, ocpi.VersionURLPath+"/locations/MY/T31/T31-LOC"+sub, "patch-token-c", b)
	}

	if statusOf("MY*T31*E1") != "AVAILABLE" || statusOf("MY*T31*E2") != "CHARGING" ||
		statusOf("MY*T31*E2:2") != "CHARGING" || availableBays() != 1 {
		t.Fatalf("the pushed statuses did not land: %s / %s / %s, %d available",
			statusOf("MY*T31*E1"), statusOf("MY*T31*E2"), statusOf("MY*T31*E2:2"), availableBays())
	}

	// The EVSE-level form: the URL names the EVSE and the body is that EVSE.
	// Both rows of a two-socket charge point move together, because what the CPO
	// reported is the state of the charge point.
	success[map[string]any](t, patch("/MY*T31*E2", ocpi.EVSE{UID: "MY*T31*E2", Status: ocpi.EVSEAvailable}))
	if statusOf("MY*T31*E2") != "AVAILABLE" || statusOf("MY*T31*E2:2") != "AVAILABLE" {
		t.Fatalf("the EVSE patch missed a socket: %s / %s", statusOf("MY*T31*E2"), statusOf("MY*T31*E2:2"))
	}
	if got := availableBays(); got != 3 {
		t.Fatalf("available bays = %d, want 3", got)
	}

	// The body form: a partial Location naming every EVSE whose status changed.
	// The spec's OUTOFORDER and RESERVED collapse onto the two statuses the
	// driver app can show.
	success[map[string]any](t, patch("", map[string]any{
		"id": "T31-LOC",
		"evse": []map[string]string{
			{"uid": "MY*T31*E1", "status": ocpi.EVSEOutOfOrder},
			{"uid": "MY*T31*E2", "status": "RESERVED"},
		},
	}))
	if got := statusOf("MY*T31*E1"); got != "INOPERATIVE" {
		t.Fatalf("OUTOFORDER became %s, want INOPERATIVE", got)
	}
	if got := statusOf("MY*T31*E2"); got != "BLOCKED" {
		t.Fatalf("RESERVED became %s, want BLOCKED", got)
	}
	if got := statusOf("MY*T31*E2:2"); got != "BLOCKED" {
		t.Fatalf("the second socket of a BLOCKED EVSE is %s, want BLOCKED", got)
	}
	if got := availableBays(); got != 0 {
		t.Fatalf("available bays = %d, want 0", got)
	}

	// All of a patch or none of it: the second EVSE here is unreadable, so the
	// first must not have moved either.
	msg := failure(t, patch("", map[string]any{
		"evse": []map[string]string{
			{"uid": "MY*T31*E1", "status": ocpi.EVSEAvailable},
			{"uid": "MY*T31*E2", "status": "SOMETHING_NEW"},
		},
	}), http.StatusBadRequest, ocpi.StatusClientError)
	if !strings.Contains(msg, "MY*T31*E2") {
		t.Fatalf("status message = %q", msg)
	}
	if got := statusOf("MY*T31*E1"); got != "INOPERATIVE" {
		t.Fatalf("a refused patch moved an EVSE anyway: %s", got)
	}

	t.Run("a patch with nothing in it is refused", func(t *testing.T) {
		failure(t, patch("", map[string]any{"id": "T31-LOC"}), http.StatusBadRequest, ocpi.StatusClientError)
	})

	t.Run("a patch for an EVSE ChargeNet does not have is a 404", func(t *testing.T) {
		failure(t, patch("/MY*T31*E9", ocpi.EVSE{UID: "MY*T31*E9", Status: ocpi.EVSEAvailable}),
			http.StatusNotFound, ocpi.StatusClientError)
	})
}

// newForeignConnector gives another partner a station and connector that list
// the same tariff id as the partner under test, which is what makes "only the
// pushing partner is repriced" a testable claim: a tariff id is the CPO's own
// string, so two partners may legitimately name a tariff the same way.
func newForeignConnector(t *testing.T, st *store.Store, partyID, tariffID string) string {
	t.Helper()
	cpoID := newFixtureCPO(t, st, partyID, "", partyID+"-token-c")
	stationID, evseUID := "stn_"+partyID, "MY*"+partyID+"*E1"
	exec := func(what, query string, args ...any) {
		t.Helper()
		if _, err := st.DB.ExecContext(context.Background(), query, args...); err != nil {
			t.Fatalf("insert %s: %v", what, err)
		}
	}
	exec("station", `INSERT INTO stations (id, cpo_id, ocpi_location_id, name, address, city,
	                       latitude, longitude, total_bays, available_bays)
	 VALUES ($1, $2, $3, 'Other Station', '9 Jalan Lain', 'Kuala Lumpur',
	         3.100000, 101.600000, 1, 1)`, stationID, cpoID, partyID+"-LOC")
	exec("connector", `INSERT INTO connectors (id, station_id, evse_uid, ocpi_connector_id,
	                           standard, format, power_type, max_power_kw, tariff_id,
	                           tariff_per_kwh, status)
	 VALUES ($1, $2, $3, '1', 'CCS2', 'CABLE', 'DC', 60.00, $4, 0, 'AVAILABLE')`,
		"con_"+partyID, stationID, evseUID, tariffID)
	t.Cleanup(func() {
		if _, err := st.DB.ExecContext(context.Background(), `DELETE FROM stations WHERE id = $1`, stationID); err != nil {
			t.Logf("clean foreign station: %v", err)
		}
	})
	return evseUID
}

// TestTariffPushRepricesConnectors covers OCPI receivers: a tariff push is
// stored in the per-kWh model and every connector of that partner which
// references it is repriced, which is how a CPO changes a price without
// re-pushing the location that carries the connector.
func TestTariffPushRepricesConnectors(t *testing.T) {
	h, st, _, _ := newReceiverServer(t)
	cpoID := newFixtureCPO(t, st, "T32", "", "tariff-token-c")
	ctx := context.Background()
	t.Cleanup(func() {
		clean := func(query string, args ...any) {
			if _, err := st.DB.ExecContext(ctx, query, args...); err != nil {
				t.Logf("clean tariff fixture: %v", err)
			}
		}
		clean(`DELETE FROM stations WHERE cpo_id = $1`, cpoID)
		clean(`DELETE FROM tariffs WHERE cpo_id = $1`, cpoID)
	})

	// The location arrives first, before the tariff it references: OCPI puts no
	// order on the modules, so the bay is stored unpriced and the tariff push
	// below is what prices it.
	location := ocpi.Location{
		CountryCode: "MY", PartyID: "T32", ID: "T32-LOC",
		Name: "Tariff Test Hub", Address: "5 Jalan Tariff", City: "Kuala Lumpur",
		Coordinates: ocpi.GeoLocation{Latitude: "3.100000", Longitude: "101.600000"},
		EVSEs: []ocpi.EVSE{{UID: "MY*T32*E1", Status: ocpi.EVSEAvailable, Connectors: []ocpi.Connector{{
			ID: "1", Standard: "IEC_62196_T2_COMBO", Format: "CABLE", PowerType: "DC",
			MaxElectricPower: 150000, TariffIDs: []string{"TAR-T32"},
		}}}},
	}
	success[ocpi.Location](t,
		do(t, h, http.MethodPut, ocpi.VersionURLPath+"/locations/MY/T32/T32-LOC", "tariff-token-c", location))

	// Another partner's connector listing the same tariff id, which must keep
	// its own price: the tariff is only the pushing partner's.
	foreignEVSE := newForeignConnector(t, st, "T33", "TAR-T32")

	body := ocpi.Tariff{
		CountryCode: "MY", PartyID: "T32", Currency: "MYR",
		Elements: []ocpi.TariffElement{
			{PriceComponents: []ocpi.PriceComponent{
				{Type: "TIME", Price: 0.05, StepSize: 60}, // a parking fee: not stored
				{Type: "ENERGY", Price: 0.85, StepSize: 1},
			}},
		},
	}
	push := func(id string, b ocpi.Tariff) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPut, ocpi.VersionURLPath+"/tariffs/MY/T32/"+id, "tariff-token-c", b)
	}

	if got := queryFloat(t, st, `SELECT tariff_per_kwh FROM connectors WHERE evse_uid = 'MY*T32*E1'`); got != 0 {
		t.Fatalf("the stalled tariff priced a connector at %v", got)
	}

	echoed := success[ocpi.Tariff](t, push("TAR-T32", body))
	if echoed.ID != "TAR-T32" {
		t.Fatalf("the pushed tariff was not named in the answer: %+v", echoed)
	}
	closeEnough(t, "the stored price", queryFloat(t, st, `SELECT price_per_kwh FROM tariffs WHERE id = 'TAR-T32'`), 0.85)
	if got := queryText(t, st, `SELECT currency FROM tariffs WHERE id = 'TAR-T32'`); got != store.CurrencyMYR {
		t.Fatalf("currency = %q, want %q", got, store.CurrencyMYR)
	}
	closeEnough(t, "the repriced connector",
		queryFloat(t, st, `SELECT tariff_per_kwh FROM connectors WHERE evse_uid = 'MY*T32*E1'`), 0.85)
	closeEnough(t, "another partner's connector",
		queryFloat(t, st, `SELECT tariff_per_kwh FROM connectors WHERE evse_uid = $1`, foreignEVSE), 0)

	// A price change is one more push and reaches the connectors on its own.
	body.Elements = []ocpi.TariffElement{
		{PriceComponents: []ocpi.PriceComponent{{Type: "ENERGY", Price: 1.10, StepSize: 1}}},
	}
	success[ocpi.Tariff](t, push("TAR-T32", body))
	closeEnough(t, "the new price", queryFloat(t, st, `SELECT price_per_kwh FROM tariffs WHERE id = 'TAR-T32'`), 1.10)
	closeEnough(t, "the re-repriced connector",
		queryFloat(t, st, `SELECT tariff_per_kwh FROM connectors WHERE evse_uid = 'MY*T32*E1'`), 1.10)

	t.Run("a tariff with no ENERGY price is refused", func(t *testing.T) {
		msg := failure(t, push("TAR-T32-TIME", ocpi.Tariff{
			CountryCode: "MY", PartyID: "T32", Currency: "MYR",
			Elements: []ocpi.TariffElement{
				{PriceComponents: []ocpi.PriceComponent{{Type: "TIME", Price: 0.05}}},
			},
		}), http.StatusBadRequest, ocpi.StatusClientError)
		if !strings.Contains(msg, "ENERGY") {
			t.Fatalf("status message = %q", msg)
		}
	})

	t.Run("a tariff in another currency is refused", func(t *testing.T) {
		msg := failure(t, push("TAR-T32-USD", ocpi.Tariff{
			CountryCode: "MY", PartyID: "T32", Currency: "USD",
			Elements: []ocpi.TariffElement{
				{PriceComponents: []ocpi.PriceComponent{{Type: "ENERGY", Price: 0.85}}},
			},
		}), http.StatusBadRequest, ocpi.StatusClientError)
		if !strings.Contains(msg, "unsupported tariff currency") {
			t.Fatalf("status message = %q", msg)
		}
	})

	t.Run("an id that contradicts the URL is refused", func(t *testing.T) {
		other := body
		other.ID = "TAR-SOMETHING-ELSE"
		failure(t, push("TAR-T32", other), http.StatusBadRequest, ocpi.StatusClientError)
	})

	t.Run("a push for another party is refused", func(t *testing.T) {
		failure(t, do(t, h, http.MethodPut, ocpi.VersionURLPath+"/tariffs/MY/XX9/TAR-T32", "tariff-token-c", body),
			http.StatusForbidden, ocpi.StatusUnauthorized)
	})

	t.Run("the tariffs module serves no reads", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/tariffs/MY/T32/TAR-T32", "tariff-token-c", nil),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
	})
}

// TestSessionPushLinksToTheChargeNetSession covers OCPI receivers: a CPO's
// Session push is matched to the ChargeNet charge it belongs to — by the
// authorization reference ChargeNet gave the CPO at the start, then by the CPO's
// own id — the two names are linked, and the numbers become the gateway event a
// driver's stream shows.
func TestSessionPushLinksToTheChargeNetSession(t *testing.T) {
	h, st, sink, _ := newReceiverServer(t)
	f := newFixtureCharge(t, st, "T40")
	push := func(id string, b ocpi.Session) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPut, ocpi.VersionURLPath+"/sessions/MY/T40/"+id, f.TokenC, b)
	}

	// The first push names ChargeNet's session in its authorization reference,
	// which is the only name the CPO was given when the charge started.
	body := ocpi.Session{
		CountryCode: "MY", PartyID: "T40", ID: "SES-T40",
		StartDateTime:          f.StartTime.Format(time.RFC3339),
		KWh:                    7.5,
		Status:                 ocpi.SessionActive,
		AuthorizationReference: f.SessionID,
		ChargeNet:              &ocpi.SessionTelemetry{CurrentPowerKW: 45, BatterySoC: 55, DurationSeconds: 600},
	}
	echoed := success[ocpi.Session](t, push("SES-T40", body))
	if echoed.ID != "SES-T40" || echoed.Status != ocpi.SessionActive {
		t.Fatalf("the push was not echoed: %+v", echoed)
	}
	if got := queryText(t, st, `SELECT ocpi_session_id FROM charging_sessions WHERE id = $1`, f.SessionID); got != "SES-T40" {
		t.Fatalf("ocpi_session_id = %q, want SES-T40", got)
	}

	ev := sink.only(t)
	if ev.SessionID != f.SessionID || ev.Status != gateway.StatusCharging || ev.Reason != "" {
		t.Fatalf("live event = %+v", ev)
	}
	// The `chargenet` block is where ChargeNet's own emulators put what OCPI has
	// no field for — how fast, how full, how long — and the driver's screen is
	// the reason it is carried through.
	if ev.State.KWhDelivered != 7.5 || ev.State.CurrentPowerKW != 45 ||
		ev.State.BatterySoC != 55 || ev.State.DurationSeconds != 600 {
		t.Fatalf("live state = %+v", ev.State)
	}
	// A push is only news: a running charge is settled when its CDR arrives, not
	// when the CPO says it is still going.
	if got := queryText(t, st, `SELECT status FROM charging_sessions WHERE id = $1`, f.SessionID); got != "CHARGING" {
		t.Fatalf("status = %q, want CHARGING", got)
	}

	// From here the CPO uses its own id and sends no ChargeNet block, which is
	// what a real CPO does: the live figures the platform has are left alone, the
	// energy is the CPO's, and the clock is the CPO's own dates.
	body.ChargeNet = nil
	body.ID = ""
	body.KWh = 9.25
	body.EndDateTime = f.StartTime.Add(15 * time.Minute).Format(time.RFC3339)
	success[ocpi.Session](t, push("SES-T40", body))
	if events := sink.events(); len(events) != 2 {
		t.Fatalf("published %d events, want 2: %+v", len(events), events)
	} else if ev = events[1]; ev.State.KWhDelivered != 9.25 || ev.State.DurationSeconds != 900 ||
		ev.State.BatterySoC != 35 || ev.State.CurrentPowerKW != 0 {
		t.Fatalf("state after a plain push = %+v", ev.State)
	}

	// The end of the charge: ChargeNet's orchestrator hears it, and it is the
	// settlement that follows which writes the money.
	body.Status = ocpi.SessionCompleted
	success[ocpi.Session](t, push("SES-T40", body))
	if ev = sink.events()[2]; ev.Status != gateway.StatusCompleted ||
		ev.Reason != simulation.ReasonRemoteStopped {
		t.Fatalf("terminal event = %+v", ev)
	}

	body.Status = ocpi.SessionInvalid
	success[ocpi.Session](t, push("SES-T40", body))
	if ev = sink.events()[3]; ev.Status != gateway.StatusFailed || ev.Reason != simulation.ReasonFault {
		t.Fatalf("invalid event = %+v", ev)
	}

	// A PATCH is the form the CPO uses once the session exists, and it carries
	// only what changed.
	patched := success[ocpi.Session](t,
		do(t, h, http.MethodPatch, ocpi.VersionURLPath+"/sessions/MY/T40/SES-T40", f.TokenC,
			map[string]any{"status": ocpi.SessionActive, "kwh": 11.5}))
	if patched.Status != ocpi.SessionActive {
		t.Fatalf("the patch was not echoed: %+v", patched)
	}
	if ev = sink.events()[4]; ev.State.KWhDelivered != 11.5 {
		t.Fatalf("the patched energy = %+v", ev.State)
	}

	quiet := func(t *testing.T, b map[string]any) {
		t.Helper()
		before := len(sink.events())
		success[ocpi.Session](t,
			do(t, h, http.MethodPut, ocpi.VersionURLPath+"/sessions/MY/T40/SES-T40", f.TokenC, b))
		if after := len(sink.events()); after != before {
			t.Fatalf("published %d events, want none", after-before)
		}
	}

	t.Run("a status that says nothing about a running charge is accepted silently", func(t *testing.T) {
		quiet(t, map[string]any{"status": ocpi.SessionPending})
	})

	t.Run("an unknown status is accepted silently", func(t *testing.T) {
		quiet(t, map[string]any{"status": "SOMETHING_NEW"})
	})

	t.Run("a push for a charge ChargeNet does not have is a 404", func(t *testing.T) {
		failure(t, push("SES-NOBODY", ocpi.Session{Status: ocpi.SessionActive}),
			http.StatusNotFound, ocpi.StatusClientError)
	})

	t.Run("the id in the body must be the one in the URL", func(t *testing.T) {
		other := ocpi.Session{ID: "SES-ELSEWHERE", Status: ocpi.SessionActive}
		failure(t, push("SES-T40", other), http.StatusBadRequest, ocpi.StatusClientError)
	})

	t.Run("another partner cannot push a charge that is not its own", func(t *testing.T) {
		newFixtureCPO(t, st, "T41", "", "T41-token-c")
		failure(t, do(t, h, http.MethodPut, ocpi.VersionURLPath+"/sessions/MY/T41/SES-T40", "T41-token-c",
			map[string]any{"status": ocpi.SessionActive}), http.StatusNotFound, ocpi.StatusClientError)
		failure(t, do(t, h, http.MethodPut, ocpi.VersionURLPath+"/sessions/MY/T41/SES-T40", f.TokenC,
			map[string]any{"status": ocpi.SessionActive}), http.StatusForbidden, ocpi.StatusUnauthorized)
	})

	t.Run("the sessions module serves no reads", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/sessions/MY/T40/SES-T40", f.TokenC, nil),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
	})
}

// TestCDRPostSettlesTheSessionOnce settles from ChargeNet's tariff, stores the
// source document with platform revenue, and prevents duplicate billing.
func TestCDRPostSettlesTheSessionOnce(t *testing.T) {
	h, st, sink, _ := newReceiverServer(t)
	f := newFixtureCharge(t, st, "T50")
	cdrPath := ocpi.VersionURLPath + "/cdrs"

	// The CDR names the session the way the CPO knows it, so the two names have
	// had to meet already.
	success[ocpi.Session](t, do(t, h, http.MethodPut,
		ocpi.VersionURLPath+"/sessions/MY/T50/SES-T50", f.TokenC,
		ocpi.Session{CountryCode: "MY", PartyID: "T50", ID: "SES-T50",
			Status: ocpi.SessionActive, AuthorizationReference: f.SessionID}))

	// The document as the CPO writes it: OCPI's own fields, its own id and its
	// own `total_cost` — and a note ChargeNet has no column for, which has to
	// survive the trip because the stored document is the CPO's record.
	body := map[string]any{
		"country_code":    "MY",
		"party_id":        "T50",
		"id":              "CDR-T50",
		"session_id":      "SES-T50",
		"start_date_time": f.StartTime.Format(time.RFC3339),
		"end_date_time":   f.StartTime.Add(30 * time.Minute).Format(time.RFC3339),
		"currency":        "MYR",
		"total_energy":    10.0,
		"total_time":      0.5, // OCPI states `total_time` in hours
		"total_cost":      map[string]any{"before_taxes": 999.00},
		"cpo_note":        "handled by the field engineer",
	}
	post := func(b map[string]any) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPost, cdrPath, f.TokenC, b)
	}

	rec := post(body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("http status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	data, code, message := envelope(t, rec)
	if code != ocpi.StatusSuccess {
		t.Fatalf("status_code = %d (%s)", code, message)
	}
	var echoed ocpi.CDR
	if err := json.Unmarshal(data, &echoed); err != nil {
		t.Fatalf("decode cdr %s: %v", data, err)
	}
	if echoed.ID != "CDR-T50" || echoed.TotalEnergy != 10 {
		t.Fatalf("the push was not echoed: %+v", echoed)
	}
	wantURL := publicBaseURL + ocpi.VersionURLPath + "/cdrs/" + ocpi.EMSPCountryCode + "/" + ocpi.EMSPPartyID + "/CDR-T50"
	if got := rec.Header().Get("Location"); got != wantURL {
		t.Fatalf("Location = %q, want %q", got, wantURL)
	}

	// Ten kWh at the fixture's 1.20/kWh is what the driver is billed, and the
	// 999.00 the CPO wrote down is not.
	closeEnough(t, "the session's cost",
		queryFloat(t, st, `SELECT total_cost FROM charging_sessions WHERE id = $1`, f.SessionID), 12.00)
	if got := queryText(t, st, `SELECT status FROM charging_sessions WHERE id = $1`, f.SessionID); got != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", got)
	}
	// `total_time` arrives in hours; the session keeps seconds.
	if got := queryInt(t, st, `SELECT duration_seconds FROM charging_sessions WHERE id = $1`, f.SessionID); got != 1800 {
		t.Fatalf("duration_seconds = %d, want 1800", got)
	}
	closeEnough(t, "the energy",
		queryFloat(t, st, `SELECT kwh_delivered FROM charging_sessions WHERE id = $1`, f.SessionID), 10.0)
	closeEnough(t, "the balance",
		queryFloat(t, st, `SELECT balance FROM wallets WHERE user_id = $1`, f.UserID), 38.00)

	// The ledger row: ChargeNet's cut of the charge, as the settlement committed
	// it.
	if got := queryInt(t, st, `SELECT count(*) FROM cdrs WHERE session_id = $1`, f.SessionID); got != 1 {
		t.Fatalf("stored %d cdr rows, want 1", got)
	}
	closeEnough(t, "gross amount",
		queryFloat(t, st, `SELECT gross_amount FROM cdrs WHERE session_id = $1`, f.SessionID), 12.00)
	closeEnough(t, "platform fee",
		queryFloat(t, st, `SELECT platform_fee FROM cdrs WHERE session_id = $1`, f.SessionID), 0.60)
	closeEnough(t, "net payout",
		queryFloat(t, st, `SELECT net_payout FROM cdrs WHERE session_id = $1`, f.SessionID), 11.40)

	// The stored document is the CPO's, plus the platform's own block.
	if got := queryText(t, st, `SELECT source_cdr_json->>'id' FROM cdrs WHERE session_id = $1`, f.SessionID); got != "CDR-T50" {
		t.Fatalf("stored cdr id = %q, want CDR-T50", got)
	}
	if got := queryText(t, st, `SELECT source_cdr_json->>'cpo_note' FROM cdrs WHERE session_id = $1`, f.SessionID); got != "handled by the field engineer" {
		t.Fatalf("the CPO's own field was dropped: %q", got)
	}
	closeEnough(t, "the revenue block's gross", queryFloat(t, st,
		`SELECT gross_amount FROM cdrs WHERE session_id=$1`, f.SessionID), 12.00)

	// The terminal event carries the amount ChargeNet billed.
	events := sink.events()
	if len(events) != 2 {
		t.Fatalf("published %d events, want 2: %+v", len(events), events)
	}
	if ev := events[1]; ev.Status != gateway.StatusCompleted ||
		ev.Reason != simulation.ReasonRemoteStopped || ev.State.Cost != 12.00 || ev.State.KWhDelivered != 10 {
		t.Fatalf("terminal event = %+v", ev)
	}

	// The CPO retries, or corrects the document, and posts it again — this time
	// naming the record in the URL as well: the charge is settled already, so
	// only the document moves and the money stays as the settlement committed it.
	body["total_time"] = 0.75
	body["total_energy"] = 12.0
	if rec := do(t, h, http.MethodPost, cdrPath+"/MY/T50/CDR-T50", f.TokenC, body); rec.Code != http.StatusCreated {
		t.Fatalf("re-post http status = %d (%s)", rec.Code, rec.Body)
	}
	closeEnough(t, "the balance after a re-post",
		queryFloat(t, st, `SELECT balance FROM wallets WHERE user_id = $1`, f.UserID), 38.00)
	if got := queryInt(t, st, `SELECT count(*) FROM cdrs WHERE session_id = $1`, f.SessionID); got != 1 {
		t.Fatalf("a re-post stored %d cdr rows, want 1", got)
	}
	closeEnough(t, "the ledger's energy after a re-post",
		queryFloat(t, st, `SELECT total_energy_kwh FROM cdrs WHERE session_id = $1`, f.SessionID), 10.0)
	if got := queryText(t, st, `SELECT source_cdr_json->>'total_time' FROM cdrs WHERE session_id = $1`, f.SessionID); got != "0.5" {
		t.Fatalf("the first source CDR was not preserved: total_time = %q", got)
	}
	// The end of the charge was published once, so a re-post is not a second one
	// as far as the driver's stream is concerned.
	if events := sink.events(); len(events) != 2 {
		t.Fatalf("a re-post published %d extra events", len(events)-2)
	}

	// The Location header is a URL the CPO can actually read: its own record,
	// with ChargeNet's share of the charge in it.
	read := do(t, h, http.MethodGet,
		ocpi.VersionURLPath+"/cdrs/"+ocpi.EMSPCountryCode+"/"+ocpi.EMSPPartyID+"/CDR-T50", f.TokenC, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read-back http status = %d (%s)", read.Code, read.Body)
	}
	raw, _, _ := envelope(t, read)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the stored cdr %s: %v", raw, err)
	}
	if _, leaked := doc["chargenet"]; leaked {
		t.Fatal("internal accounting leaked into source OCPI CDR")
	}

	if doc["cpo_note"] != "handled by the field engineer" {
		t.Fatalf("the read-back lost the CPO's own field: %v", doc)
	}

	t.Run("a CDR for a charge ChargeNet does not have is a 404", func(t *testing.T) {
		failure(t, post(map[string]any{
			"country_code": "MY", "party_id": "T50", "id": "CDR-T50-B", "session_id": "SES-NOBODY",
		}), http.StatusNotFound, ocpi.StatusClientError)
	})

	t.Run("a CDR that names no session is refused", func(t *testing.T) {
		failure(t, post(map[string]any{"country_code": "MY", "party_id": "T50", "id": "CDR-T50-C"}),
			http.StatusBadRequest, ocpi.StatusClientError)
	})

	t.Run("the id in the body must be the one in the URL", func(t *testing.T) {
		failure(t, do(t, h, http.MethodPost, cdrPath+"/MY/T50/CDR-T50", f.TokenC, map[string]any{
			"country_code": "MY", "party_id": "T50", "id": "CDR-ELSEWHERE", "session_id": "SES-T50",
		}), http.StatusBadRequest, ocpi.StatusClientError)
	})

	t.Run("a CDR for another party is refused", func(t *testing.T) {
		failure(t, post(map[string]any{
			"country_code": "MY", "party_id": "XX9", "id": "CDR-T50-D", "session_id": "SES-T50",
		}), http.StatusForbidden, ocpi.StatusUnauthorized)
	})

	t.Run("another partner cannot read the record", func(t *testing.T) {
		newFixtureCPO(t, st, "T51", "", "T51-token-c")
		failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/cdrs/MY/CNT/CDR-T50", "T51-token-c", nil),
			http.StatusNotFound, ocpi.StatusClientError)
	})

	t.Run("the read URL names ChargeNet's own party", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/cdrs/MY/T50/CDR-T50", f.TokenC, nil),
			http.StatusForbidden, ocpi.StatusUnauthorized)
	})

	t.Run("the cdrs module only takes a POST", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, cdrPath, f.TokenC, nil),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
		// The mux handles methods the module does not advertise.
		if rec := do(t, h, http.MethodDelete, cdrPath, f.TokenC, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE http status = %d, want 405 (%s)", rec.Code, rec.Body)
		}
	})
}

// TestCommandResultReachesTheGateway covers OCPI receivers: the asynchronous
// answer a CPO posts once a command ChargeNet sent it has landed is handed to
// the gateway waiting for it, and echoed back as OCPI has it.
func TestCommandResultReachesTheGateway(t *testing.T) {
	h, st, _, commands := newReceiverServer(t)
	newFixtureCPO(t, st, "T52", "", "T52-token-c")
	callback := ocpi.VersionURLPath + "/commands/" + ocpi.CommandStopSession + "/SES-T52"

	// The CPO answers long after the response that accepted the command.
	rec := do(t, h, http.MethodPost, callback, "T52-token-c",
		ocpi.CommandResponse{Result: ocpi.CommandAccepted})
	if rec.Code != http.StatusOK {
		t.Fatalf("http status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	data, code, message := envelope(t, rec)
	if code != ocpi.StatusSuccess {
		t.Fatalf("status_code = %d (%s)", code, message)
	}
	var echoed ocpi.CommandResponse
	if err := json.Unmarshal(data, &echoed); err != nil || echoed.Result != ocpi.CommandAccepted {
		t.Fatalf("echoed %s (%v)", data, err)
	}
	got := commands.results()
	if len(got) != 1 {
		t.Fatalf("the gateway was handed %d results, want 1", len(got))
	}
	if got[0].Command != ocpi.CommandStopSession || got[0].UID != "SES-T52" ||
		got[0].Result.Result != ocpi.CommandAccepted {
		t.Fatalf("the gateway was handed %+v", got[0])
	}

	// A rejection carries the CPO's reason with it: whoever is watching the
	// charge is told why it never started.
	rejected := ocpi.CommandResponse{Result: ocpi.CommandRejected, Message: "the driver walked away"}
	success[ocpi.CommandResponse](t, do(t, h, http.MethodPost,
		ocpi.VersionURLPath+"/commands/"+ocpi.CommandStartSession+"/SES-T52", "T52-token-c", rejected))
	if got = commands.results(); len(got) != 2 || got[1].Result != rejected {
		t.Fatalf("the gateway was handed %+v", got)
	}

	t.Run("a command ChargeNet never sends is a 3001", func(t *testing.T) {
		failure(t, do(t, h, http.MethodPost, ocpi.VersionURLPath+"/commands/RESERVE_NOW/SES-T52",
			"T52-token-c", ocpi.CommandResponse{Result: ocpi.CommandAccepted}),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
		if n := len(commands.results()); n != 2 {
			t.Fatalf("a refused command was handed on anyway: %d results", n)
		}
	})

	t.Run("the commands module serves no reads", func(t *testing.T) {
		failure(t, do(t, h, http.MethodGet, callback, "T52-token-c", nil),
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI)
	})

	t.Run("a callback needs both a command and a uid", func(t *testing.T) {
		failure(t, do(t, h, http.MethodPost, ocpi.VersionURLPath+"/commands/"+ocpi.CommandStopSession,
			"T52-token-c", ocpi.CommandResponse{Result: ocpi.CommandAccepted}),
			http.StatusNotFound, ocpi.StatusClientError)
	})

	t.Run("another token cannot answer the command", func(t *testing.T) {
		failure(t, do(t, h, http.MethodPost, callback, "T52-token-a", ocpi.CommandResponse{}),
			http.StatusUnauthorized, ocpi.StatusUnauthorized)
	})

	t.Run("with no gateway running the answer has nowhere to go", func(t *testing.T) {
		// Same receiver, but nothing is listening on the other side, so no
		// command of ChargeNet's can be outstanding.
		bare := ocpi.New(config.Config{PublicBaseURL: publicBaseURL, PlatformFeePct: 5}, st)
		failure(t, do(t, bare.Handler(), http.MethodPost, callback, "T52-token-c",
			ocpi.CommandResponse{Result: ocpi.CommandAccepted}),
			http.StatusNotFound, ocpi.StatusClientError)
	})
}

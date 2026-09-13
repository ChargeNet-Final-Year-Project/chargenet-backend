package emulator_test

// the emulators integration tests: the CPO emulators /the emulators. Each test drives a
// real emulator (its real endpoints, its real session runner) against a fake
// ChargeNet that records every push, and — for the session path — reads the
// connector it charges from the shared test database, exactly as the platform
// seeds it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/ocpi/emulator"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// push is one request an emulator made to ChargeNet, as ChargeNet saw it.
type push struct {
	Method string
	Path   string
	Token  string
	Body   map[string]any
}

// fakeChargeNet is ChargeNet's OCPI surface in miniature: it answers every push
// with a success envelope and records what arrived, so the emulator's outbound
// half can be asserted without the real receiver.
type fakeChargeNet struct {
	mu    sync.Mutex
	calls []push
}

func newFakeChargeNet(t *testing.T) (*fakeChargeNet, *httptest.Server) {
	t.Helper()
	f := &fakeChargeNet{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeChargeNet) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, push{Method: r.Method, Path: r.URL.Path, Token: r.Header.Get("Authorization"), Body: body})
	f.mu.Unlock()

	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/credentials") {
		ocpi.OK(w, http.StatusOK, ocpi.Credentials{Token: "chargenet-token-c", URL: "http://charge.net/ocpi/emsp/versions"})
		return
	}
	ocpi.OK(w, http.StatusOK, map[string]any{})
}

func (f *fakeChargeNet) all() []push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]push(nil), f.calls...)
}

// await waits for a push matching want, failing the test if none arrives.
func (f *fakeChargeNet) await(t *testing.T, what string, want func(push) bool) push {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.all() {
			if want(c) {
				return c
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; saw %+v", what, f.all())
	return push{}
}

// newEmulator builds one emulator for profile, pointing its pushes at origin,
// and serves its endpoints on an httptest server. Sessions tick fast so a test
// does not wait, and the heartbeat is off unless a test turns it on.
func newEmulator(t *testing.T, st *store.Store, prof emulator.Profile, origin string, opts ...emulator.Option) (*emulator.Emulator, *httptest.Server) {
	t.Helper()
	base := []emulator.Option{
		emulator.WithOrigin(origin),
		emulator.WithTimeScale(1),
		emulator.WithTick(5 * time.Millisecond),
		emulator.WithHeartbeat(0),
	}
	e := emulator.New(config.Config{PublicBaseURL: origin, SimTimeScale: 1, PlatformFeePct: 5}, st, prof, append(base, opts...)...)
	mux := http.NewServeMux()
	e.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		e.Close()
		srv.Close()
	})
	return e, srv
}

// do performs one HTTP request with `Authorization: Token <token>`.
func do(t *testing.T, url, method, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Token "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test reads it all
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// successData decodes an OCPI success envelope and returns its payload.
func successData[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var env struct {
		Data          T      `json:"data"`
		StatusCode    int    `json:"status_code"`
		StatusMessage string `json:"status_message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %q: %v", raw, err)
	}
	if env.StatusCode != ocpi.StatusSuccess {
		t.Fatalf("status_code = %d (%s)", env.StatusCode, env.StatusMessage)
	}
	return env.Data
}

// voltgoEVSE finds one seeded VoltGo connector the emulator can charge on.
func voltgoEVSE(t *testing.T, st *store.Store) (evseUID, locationID, connectorID string) {
	t.Helper()
	err := st.DB.QueryRowContext(context.Background(), `
		SELECT k.evse_uid, s.ocpi_location_id, coalesce(k.ocpi_connector_id, '1')
		  FROM connectors k JOIN stations s ON s.id = k.station_id
		 WHERE s.cpo_id = 'cpo_voltgo' AND s.ocpi_location_id IS NOT NULL
		 ORDER BY k.evse_uid LIMIT 1`).Scan(&evseUID, &locationID, &connectorID)
	if err != nil {
		t.Fatalf("find a VoltGo EVSE: %v", err)
	}
	return evseUID, locationID, connectorID
}

// startBody builds a START_SESSION payload for one connector.
func startBody(responseURL, locationID, evseUID, connectorID, ref string) map[string]any {
	return map[string]any{
		"response_url":            responseURL,
		"token":                   map[string]any{"uid": "usr_test", "type": "APP_USER"},
		"location_id":             locationID,
		"evse_uid":                evseUID,
		"connector_id":            connectorID,
		"authorization_reference": ref,
	}
}

func TestEmulatorServesItsNetwork(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, srv := newEmulator(t, st, emulator.Profiles[0], "http://charge.net")

	const base = "/emulator/voltgo/ocpi"

	// The seeded token_b opens it.
	status, raw := do(t, srv.URL+base+"/versions", http.MethodGet, "voltgo-token-b-dev", nil)
	if status != http.StatusOK {
		t.Fatalf("GET versions = %d (%s)", status, raw)
	}
	versions := successData[[]ocpi.Version](t, raw)
	if len(versions) != 1 || versions[0].Version != ocpi.VersionNumber {
		t.Fatalf("versions = %+v", versions)
	}

	status, raw = do(t, srv.URL+base+"/2.3.0/locations", http.MethodGet, "voltgo-token-b-dev", nil)
	if status != http.StatusOK {
		t.Fatalf("GET locations = %d (%s)", status, raw)
	}
	locs := successData[[]ocpi.Location](t, raw)
	if len(locs) == 0 {
		t.Fatal("no locations served")
	}
	for _, loc := range locs {
		if loc.CountryCode != "MY" || loc.PartyID != "VLT" {
			t.Fatalf("location party = %s/%s", loc.CountryCode, loc.PartyID)
		}
		if len(loc.EVSEs) == 0 || len(loc.EVSEs[0].Connectors) == 0 {
			t.Fatalf("location %s has no EVSE/connector: %+v", loc.ID, loc)
		}
	}

	status, raw = do(t, srv.URL+base+"/2.3.0/tariffs", http.MethodGet, "voltgo-token-b-dev", nil)
	if status != http.StatusOK {
		t.Fatalf("GET tariffs = %d (%s)", status, raw)
	}
	tariffs := successData[[]ocpi.Tariff](t, raw)
	if len(tariffs) == 0 || tariffs[0].Currency != "MYR" {
		t.Fatalf("tariffs = %+v", tariffs)
	}

	// A wrong or missing token is turned away with OCPI 2001.
	for _, token := range []string{"", "not-the-token"} {
		status, raw = do(t, srv.URL+base+"/2.3.0/locations", http.MethodGet, token, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("GET locations with token %q = %d, want 401", token, status)
		}
		var env struct {
			StatusCode int `json:"status_code"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.StatusCode != ocpi.StatusUnauthorized {
			t.Fatalf("unauthorized status_code = %d", env.StatusCode)
		}
	}
}

func TestEmulatorRunsASession(t *testing.T) {
	st := testutil.NewTestStore(t)
	fake, chargeNet := newFakeChargeNet(t)
	evse, loc, conn := voltgoEVSE(t, st)
	_, srv := newEmulator(t, st, emulator.Profiles[0], chargeNet.URL)

	const base = "/emulator/voltgo/ocpi"

	status, raw := do(t, srv.URL+base+"/2.3.0/commands/START_SESSION", http.MethodPost, "voltgo-token-b-dev",
		startBody(chargeNet.URL+"/ocpi/emsp/2.3.0/commands/START_SESSION/csn_test", loc, evse, conn, "csn_test"))
	if status != http.StatusOK {
		t.Fatalf("START_SESSION = %d (%s)", status, raw)
	}
	if res := successData[ocpi.CommandResponse](t, raw); res.Result != ocpi.CommandAccepted {
		t.Fatalf("start result = %q, want ACCEPTED", res.Result)
	}

	// The EVSE goes CHARGING and the Session is pushed while it runs. Every
	// push must authenticate with the partner's token_c.
	charging := fake.await(t, "an EVSE CHARGING push", func(p push) bool {
		return p.Method == http.MethodPatch && strings.Contains(p.Path, "/locations/") && p.Body["status"] == "CHARGING"
	})
	if charging.Token != "Token voltgo-token-c-dev" {
		t.Fatalf("push token = %q, want token_c", charging.Token)
	}
	active := fake.await(t, "an ACTIVE session push", func(p push) bool {
		return p.Method == http.MethodPut && strings.Contains(p.Path, "/sessions/") && p.Body["status"] == "ACTIVE"
	})
	sessionID, _ := active.Body["id"].(string)
	if sessionID == "" {
		t.Fatalf("session push has no id: %+v", active.Body)
	}
	if active.Body["authorization_reference"] != "csn_test" {
		t.Fatalf("authorization_reference = %v", active.Body["authorization_reference"])
	}

	// Stopping it produces the final Session, the CDR and the freed EVSE.
	status, raw = do(t, srv.URL+base+"/2.3.0/commands/STOP_SESSION", http.MethodPost, "voltgo-token-b-dev",
		map[string]any{
			"response_url": chargeNet.URL + "/ocpi/emsp/2.3.0/commands/STOP_SESSION/" + sessionID,
			"session_id":   sessionID,
		})
	if status != http.StatusOK {
		t.Fatalf("STOP_SESSION = %d (%s)", status, raw)
	}
	if res := successData[ocpi.CommandResponse](t, raw); res.Result != ocpi.CommandAccepted {
		t.Fatalf("stop result = %q", res.Result)
	}

	fake.await(t, "a COMPLETED session push", func(p push) bool {
		return p.Method == http.MethodPut && strings.Contains(p.Path, "/sessions/") && p.Body["status"] == "COMPLETED"
	})
	cdr := fake.await(t, "a CDR push", func(p push) bool {
		return p.Method == http.MethodPost && strings.HasSuffix(p.Path, "/cdrs")
	})
	if cdr.Body["session_id"] != sessionID {
		t.Fatalf("cdr session_id = %v, want %s", cdr.Body["session_id"], sessionID)
	}
	if cdr.Body["country_code"] != "MY" || cdr.Body["party_id"] != "VLT" {
		t.Fatalf("cdr party = %v/%v", cdr.Body["country_code"], cdr.Body["party_id"])
	}
	fake.await(t, "the EVSE AVAILABLE push", func(p push) bool {
		return p.Method == http.MethodPatch && strings.Contains(p.Path, "/locations/") && p.Body["status"] == "AVAILABLE"
	})
}

func TestEmulatorOfflineAndReject(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, chargeNet := newFakeChargeNet(t)
	evse, loc, conn := voltgoEVSE(t, st)
	e, srv := newEmulator(t, st, emulator.Profiles[0], chargeNet.URL)

	const base = "/emulator/voltgo/ocpi"
	body := startBody(chargeNet.URL+"/cb", loc, evse, conn, "csn_x")

	// OFFLINE: every command answers 503, so ChargeNet sees CPO_UNREACHABLE.
	e.SetOffline(true)
	if status, _ := do(t, srv.URL+base+"/2.3.0/commands/START_SESSION", http.MethodPost, "voltgo-token-b-dev", body); status != http.StatusServiceUnavailable {
		t.Fatalf("START_SESSION while OFFLINE = %d, want 503", status)
	}

	// REJECT_NEXT: one start is turned down, then the knob is spent.
	e.SetOffline(false)
	e.RejectNext()
	status, raw := do(t, srv.URL+base+"/2.3.0/commands/START_SESSION", http.MethodPost, "voltgo-token-b-dev", body)
	if status != http.StatusOK {
		t.Fatalf("START_SESSION REJECT_NEXT = %d (%s)", status, raw)
	}
	if res := successData[ocpi.CommandResponse](t, raw); res.Result != ocpi.CommandRejected {
		t.Fatalf("result = %q, want REJECTED", res.Result)
	}
}

func TestEmulatorPushesItsNetworkOnBoot(t *testing.T) {
	st := testutil.NewTestStore(t)
	fake, chargeNet := newFakeChargeNet(t)
	e, _ := newEmulator(t, st, emulator.Profiles[0], chargeNet.URL, emulator.WithHeartbeat(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Run(ctx)

	fake.await(t, "a location push", func(p push) bool {
		return p.Method == http.MethodPut && strings.Contains(p.Path, "/locations/")
	})
	fake.await(t, "a tariff push", func(p push) bool {
		return p.Method == http.MethodPut && strings.Contains(p.Path, "/tariffs/")
	})
}

func TestEmulatorRegistersWithChargeNet(t *testing.T) {
	st := testutil.NewTestStore(t)
	fake, chargeNet := newFakeChargeNet(t)
	e, _ := newEmulator(t, st, emulator.Profiles[0], chargeNet.URL)

	if err := e.Register(context.Background(), "voltgo-token-a-dev"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The spec flow: versions, version details, then POST credentials
	// carrying our own token_b — authenticated with Token A.
	fake.await(t, "GET versions", func(p push) bool {
		return p.Method == http.MethodGet && strings.HasSuffix(p.Path, "/versions")
	})
	fake.await(t, "GET version details", func(p push) bool {
		return p.Method == http.MethodGet && strings.HasSuffix(p.Path, "/2.3.0/")
	})
	creds := fake.await(t, "POST credentials", func(p push) bool {
		return p.Method == http.MethodPost && strings.HasSuffix(p.Path, "/credentials")
	})
	if creds.Token != "Token voltgo-token-a-dev" {
		t.Fatalf("credentials presented %q, want Token A", creds.Token)
	}
	if creds.Body["token"] != "voltgo-token-b-dev" {
		t.Fatalf("credentials body token = %v, want token_b", creds.Body["token"])
	}

	// The handshake is followed by the network push.
	fake.await(t, "a location push after registration", func(p push) bool {
		return p.Method == http.MethodPut && strings.Contains(p.Path, "/locations/")
	})
}

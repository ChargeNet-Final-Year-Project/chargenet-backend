package ocpi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

const publicBaseURL = "http://localhost:8080"

// voltgoTokenC is the operational token of the seeded VoltGo partner (VLT).
const voltgoTokenC = "voltgo-token-c-dev"

// newServer wires the OCPI endpoints against the shared test database.
func newServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	return ocpi.New(config.Config{PublicBaseURL: publicBaseURL, PlatformFeePct: 5}, st).Handler(), st
}

// do sends one OCPI request: a non-empty token is presented as
// `Authorization: Token <token>`.
func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	header := ""
	if token != "" {
		header = "Token " + token
	}
	return send(t, h, method, path, header, body)
}

// bearer drives the mobile API, which authenticates with `Bearer <jwt>`.
func bearer(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	header := ""
	if token != "" {
		header = "Bearer " + token
	}
	return send(t, h, method, path, header, body)
}

// send performs one request with a pre-formatted Authorization header.
func send(t *testing.T, h http.Handler, method, path, authHeader string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// success decodes an OCPI success envelope and returns its data payload.
func success[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("http status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var env struct {
		Data          T      `json:"data"`
		StatusCode    int    `json:"status_code"`
		StatusMessage string `json:"status_message"`
		Timestamp     string `json:"timestamp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	if env.StatusCode != ocpi.StatusSuccess {
		t.Fatalf("status_code = %d (%s)", env.StatusCode, env.StatusMessage)
	}
	if env.Timestamp == "" {
		t.Fatalf("envelope has no timestamp: %s", rec.Body)
	}
	return env.Data
}

// failure asserts a failure envelope and returns its status message.
func failure(t *testing.T, rec *httptest.ResponseRecorder, wantHTTP, wantCode int) string {
	t.Helper()
	if rec.Code != wantHTTP {
		t.Fatalf("http status = %d, want %d (%s)", rec.Code, wantHTTP, rec.Body)
	}
	var env struct {
		Data          map[string]any `json:"data"`
		StatusCode    int            `json:"status_code"`
		StatusMessage string         `json:"status_message"`
		Timestamp     string         `json:"timestamp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode failure envelope %q: %v", rec.Body.String(), err)
	}
	if env.StatusCode != wantCode {
		t.Fatalf("status_code = %d (%s), want %d", env.StatusCode, env.StatusMessage, wantCode)
	}
	if env.StatusMessage == "" || env.Timestamp == "" {
		t.Fatalf("incomplete failure envelope: %s", rec.Body)
	}
	return env.StatusMessage
}

// newFixtureCPO inserts a partner for one test and deletes it afterwards.
// testutil only truncates the driver tables, so cpo_partners fixture rows would
// otherwise collide with cpo_partners' UNIQUE(country_code, party_id) on the next
// run. tokenA/tokenC may be empty to leave that credential unset.
func newFixtureCPO(t *testing.T, st *store.Store, partyID, tokenA, tokenC string) string {
	t.Helper()
	id := "cpo_test_" + strings.ToLower(partyID)
	clean := func() {
		// What a test attached to the partner goes first: a pushed tariff, a
		// pushed location (with its connectors) or the placeholder tariff a
		// location's unpushed tariff id leaves behind all reference it.
		for _, query := range []string{
			`DELETE FROM tariffs WHERE cpo_id = $1`,
			`DELETE FROM stations WHERE cpo_id = $1`,
			`DELETE FROM cpo_partners WHERE id = $1`,
		} {
			if _, err := st.DB.ExecContext(context.Background(), query, id); err != nil {
				t.Logf("clean fixture cpo: %v", err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	if _, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO cpo_partners (id, name, slug, country_code, party_id, base_url,
		                           contact_email, token_a, token_c, handshake_status, status)
		 VALUES ($1, 'Fixture CPO', $1, 'MY', $2, 'http://fixture.test/ocpi',
		         'ops@fixture.test', $3, $4, 'NOT_STARTED', 'PENDING')`,
		id, partyID, nullIfEmpty(tokenA), nullIfEmpty(tokenC)); err != nil {
		t.Fatalf("insert fixture cpo: %v", err)
	}
	if tokenC != "" {
		// A partner that already holds a Token C is registered.
		if _, err := st.DB.ExecContext(context.Background(),
			`UPDATE cpo_partners SET handshake_status = 'VERIFIED', status = 'ACTIVE' WHERE id = $1`,
			id); err != nil {
			t.Fatalf("activate fixture cpo: %v", err)
		}
	}
	return id
}

// nullIfEmpty stores an unset credential as SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestVersionsListsChargeNetVersion(t *testing.T) {
	h, _ := newServer(t)

	versions := success[[]ocpi.Version](t, do(t, h, http.MethodGet, ocpi.VersionsURLPath, voltgoTokenC, nil))
	if len(versions) != 1 {
		t.Fatalf("versions = %+v, want one supported version", versions)
	}
	if versions[0].Version != ocpi.VersionNumber {
		t.Fatalf("version = %q, want %q", versions[0].Version, ocpi.VersionNumber)
	}
	if want := publicBaseURL + ocpi.VersionURLPath + "/"; versions[0].URL != want {
		t.Fatalf("versions url = %q, want %q", versions[0].URL, want)
	}
}

func TestVersionDetailsListsEveryModule(t *testing.T) {
	h, _ := newServer(t)

	details := success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/", voltgoTokenC, nil))
	if details.Version != ocpi.VersionNumber {
		t.Fatalf("version details version = %q", details.Version)
	}

	want := []string{
		ocpi.ModuleCredentials,
		ocpi.ModuleLocations,
		ocpi.ModuleTariffs,
		ocpi.ModuleSessions,
		ocpi.ModuleCDRs,
		ocpi.ModuleCommands,
	}
	if len(details.Endpoints) != len(want) {
		t.Fatalf("endpoints = %+v, want %d of them", details.Endpoints, len(want))
	}
	for i, module := range want {
		got := details.Endpoints[i]
		if got.Identifier != module {
			t.Fatalf("endpoints[%d] = %q, want %q", i, got.Identifier, module)
		}
		if got.Role != ocpi.InterfaceReceiver {
			t.Fatalf("%s role = %q, want %q", module, got.Role, ocpi.InterfaceReceiver)
		}
		if wantURL := publicBaseURL + ocpi.VersionURLPath + "/" + module; got.URL != wantURL {
			t.Fatalf("%s url = %q, want %q", module, got.URL, wantURL)
		}
	}

	// The bare path answers as well, so ServeMux never redirects a CPO client.
	success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, ocpi.VersionURLPath, voltgoTokenC, nil))

	// A path that is no OCPI module at all must not be answered with the
	// endpoint list either: a CPO that typo'd its URL deserves a 404.
	failure(t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/hub", voltgoTokenC, nil),
		http.StatusNotFound, ocpi.StatusClientError)
}

// TestModuleEndpointsEnforceTheTokenContract checks Token C across every
// advertised module and OCPI 2001 for all other credentials.
func TestModuleEndpointsEnforceTheTokenContract(t *testing.T) {
	h, st := newServer(t)
	// A partner that is registered (Token C) and still holds a Token A.
	newFixtureCPO(t, st, "TS7", "unspent-token-a", "module-test-token-c")

	// Every answer below is the receiver's own, which is how the test tells "the
	// token was accepted" from "the token was rejected": 2001 never appears.
	accepted := []struct {
		name     string
		method   string
		path     string
		wantHTTP int
		wantCode int
	}{
		{"locations read", http.MethodGet, ocpi.VersionURLPath + "/locations",
			http.StatusNotImplemented, ocpi.StatusUnableToUseAPI},
		{"locations push", http.MethodPut, ocpi.VersionURLPath + "/locations/MY/TS7/LOC1",
			http.StatusBadRequest, ocpi.StatusClientError},
		{"locations evse patch", http.MethodPatch, ocpi.VersionURLPath + "/locations/MY/TS7/LOC1/EVSE1",
			http.StatusBadRequest, ocpi.StatusClientError},
		{"tariffs push", http.MethodPut, ocpi.VersionURLPath + "/tariffs/MY/TS7/TAR1",
			http.StatusBadRequest, ocpi.StatusClientError},
		{"sessions push", http.MethodPatch, ocpi.VersionURLPath + "/sessions/MY/TS7/SES1",
			http.StatusBadRequest, ocpi.StatusClientError},
		{"cdrs post", http.MethodPost, ocpi.VersionURLPath + "/cdrs",
			http.StatusBadRequest, ocpi.StatusClientError},
		// The CDR read URL is the eMSP's own endpoint — what the `Location`
		// header of the POST answers with — so it names ChargeNet's party rather
		// than the pushing CPO's, which is the one every other module checks.
		{"cdr read", http.MethodGet,
			ocpi.VersionURLPath + "/cdrs/" + ocpi.EMSPCountryCode + "/" + ocpi.EMSPPartyID + "/CDR1",
			http.StatusNotFound, ocpi.StatusClientError},
		{"command result", http.MethodPost, ocpi.VersionURLPath + "/commands/START_SESSION/cmd1",
			http.StatusBadRequest, ocpi.StatusClientError},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			failure(t, do(t, h, tc.method, tc.path, "module-test-token-c", nil), tc.wantHTTP, tc.wantCode)
		})
	}

	rejected := []struct{ name, method, path, token string }{
		{"garbage token", http.MethodPut, ocpi.VersionURLPath + "/locations/MY/TS7/LOC1", "not-a-real-token"},
		{"garbage token read", http.MethodGet, ocpi.VersionURLPath + "/locations", "not-a-real-token"},
		{"base64 garbage token", http.MethodGet, ocpi.VersionURLPath + "/locations",
			base64.StdEncoding.EncodeToString([]byte("still-not-a-real-token"))},
		{"no token", http.MethodPost, ocpi.VersionURLPath + "/cdrs", ""},
		// Token A registers; it opens versions and POST credentials only.
		{"token a", http.MethodPut, ocpi.VersionURLPath + "/locations/MY/TS7/LOC1", "unspent-token-a"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			failure(t, do(t, h, tc.method, tc.path, tc.token, nil),
				http.StatusUnauthorized, ocpi.StatusUnauthorized)
		})
	}
}

func TestMissingOrUnknownTokenIsRejected(t *testing.T) {
	h, _ := newServer(t)

	cases := []struct{ name, header string }{
		{"no header", ""},
		{"wrong scheme", "Bearer " + voltgoTokenC},
		{"empty token", "Token "},
		{"unknown token", "Token not-a-real-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, ocpi.VersionsURLPath, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			failure(t, rec, http.StatusUnauthorized, ocpi.StatusUnauthorized)
		})
	}

	// Reading ChargeNet's credentials needs a registered partner too.
	failure(t, do(t, h, http.MethodGet, ocpi.CredentialsPath, "not-a-real-token", nil),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)
}

func TestGetCredentialsReturnsChargeNetIdentity(t *testing.T) {
	h, _ := newServer(t)

	creds := success[ocpi.Credentials](t, do(t, h, http.MethodGet, ocpi.CredentialsPath, voltgoTokenC, nil))
	if creds.Token != voltgoTokenC {
		t.Fatalf("token = %q, want the presented one", creds.Token)
	}
	if want := publicBaseURL + ocpi.VersionsURLPath; creds.URL != want {
		t.Fatalf("url = %q, want %q", creds.URL, want)
	}
	if len(creds.Roles) != 1 {
		t.Fatalf("roles = %+v, want one", creds.Roles)
	}
	role := creds.Roles[0]
	if role.Role != ocpi.RoleEMSP || role.PartyID != ocpi.EMSPPartyID || role.CountryCode != ocpi.EMSPCountryCode {
		t.Fatalf("role = %+v", role)
	}
	if role.BusinessDetails.Name != ocpi.EMSPName {
		t.Fatalf("business_details.name = %q, want %q", role.BusinessDetails.Name, ocpi.EMSPName)
	}

	// Some CPO implementations base64-encode the token.
	encoded := base64.StdEncoding.EncodeToString([]byte(voltgoTokenC))
	again := success[ocpi.Credentials](t, do(t, h, http.MethodGet, ocpi.CredentialsPath, encoded, nil))
	if again.Token != voltgoTokenC {
		t.Fatalf("base64 token resolved to %q, want %q", again.Token, voltgoTokenC)
	}
}

func TestPostCredentialsCompletesRegistration(t *testing.T) {
	h, st := newServer(t)
	id := newFixtureCPO(t, st, "TS1", "reg-token-a", "")
	ctx := context.Background()

	// The handshake a CPO performs before registering: read the versions and
	// the version details with the Token A the portal gave it.
	success[[]ocpi.Version](t, do(t, h, http.MethodGet, ocpi.VersionsURLPath, "reg-token-a", nil))
	success[ocpi.VersionDetails](t, do(t, h, http.MethodGet, ocpi.VersionURLPath+"/", "reg-token-a", nil))

	body := ocpi.Credentials{
		Token: "fresh-token-b",
		URL:   "http://fixture.test/emulator/ts1/ocpi/versions",
		Roles: []ocpi.CredentialsRole{{
			Role:            ocpi.RoleCPO,
			PartyID:         "TS1",
			CountryCode:     "MY",
			BusinessDetails: ocpi.BusinessDetails{Name: "Fixture CPO"},
		}},
	}

	creds := success[ocpi.Credentials](t, do(t, h, http.MethodPost, ocpi.CredentialsPath, "reg-token-a", body))
	if len(creds.Token) != 64 {
		t.Fatalf("Token C = %q, want 64 hex characters", creds.Token)
	}
	if _, err := hex.DecodeString(creds.Token); err != nil {
		t.Fatalf("Token C %q is not hex: %v", creds.Token, err)
	}
	if want := publicBaseURL + ocpi.VersionsURLPath; creds.URL != want {
		t.Fatalf("credentials url = %q, want %q", creds.URL, want)
	}

	cpo, err := st.GetCPO(ctx, id)
	if err != nil {
		t.Fatalf("get cpo: %v", err)
	}
	if cpo.TokenA != "" || cpo.TokenAUsedAt == nil {
		t.Fatalf("Token A survived the exchange: token_a=%q used_at=%v", cpo.TokenA, cpo.TokenAUsedAt)
	}
	if cpo.TokenB != "fresh-token-b" {
		t.Fatalf("token_b = %q, want the token the CPO sent", cpo.TokenB)
	}
	if cpo.TokenC != creds.Token {
		t.Fatalf("token_c = %q, want the token ChargeNet answered with (%q)", cpo.TokenC, creds.Token)
	}
	if want := "http://fixture.test/emulator/ts1/ocpi"; cpo.BaseURL != want {
		t.Fatalf("base_url = %q, want the CPO's OCPI root (%q)", cpo.BaseURL, want)
	}
	if cpo.HandshakeStatus != store.HandshakeInProgress {
		t.Fatalf("handshake_status = %q, want %q", cpo.HandshakeStatus, store.HandshakeInProgress)
	}
	if cpo.Status != store.CPOStatusPending {
		t.Fatalf("registration bypassed approval: status = %q", cpo.Status)
	}
	if err = st.SetHandshakeResult(ctx, id, store.HandshakeVerified, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	cpo, err = st.GetCPO(ctx, id)
	if err != nil || cpo.Status != store.CPOStatusPending {
		t.Fatal("connection verification bypassed approval")
	}
	failure(t, do(t, h, http.MethodGet, ocpi.CredentialsPath, creds.Token, nil), http.StatusUnauthorized, ocpi.StatusUnauthorized)
	if err = st.SetCPOStatus(ctx, id, store.CPOStatusActive, nil); err != nil {
		t.Fatal(err)
	}

	// Token A is single use: replaying the registration is rejected, and the
	// spent token no longer opens the read-only endpoints either.
	failure(t, do(t, h, http.MethodPost, ocpi.CredentialsPath, "reg-token-a", body),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)
	failure(t, do(t, h, http.MethodGet, ocpi.VersionsURLPath, "reg-token-a", nil),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)

	// The issued Token C is what the partner calls ChargeNet with from now on.
	got := success[ocpi.Credentials](t, do(t, h, http.MethodGet, ocpi.CredentialsPath, creds.Token, nil))
	if got.Token != creds.Token {
		t.Fatalf("credentials token = %q, want %q", got.Token, creds.Token)
	}
}

func TestPostCredentialsValidatesBody(t *testing.T) {
	h, st := newServer(t)
	id := newFixtureCPO(t, st, "TS2", "val-token-a", "")

	role := ocpi.CredentialsRole{Role: ocpi.RoleCPO, PartyID: "TS2", CountryCode: "MY"}
	cases := []struct {
		name string
		body ocpi.Credentials
	}{
		{"no token", ocpi.Credentials{URL: "http://fixture.test/ocpi/versions", Roles: []ocpi.CredentialsRole{role}}},
		{"no url", ocpi.Credentials{Token: "b", Roles: []ocpi.CredentialsRole{role}}},
		{"relative url", ocpi.Credentials{Token: "b", URL: "/ocpi/versions", Roles: []ocpi.CredentialsRole{role}}},
		{"foreign party", ocpi.Credentials{
			Token: "b",
			URL:   "http://fixture.test/ocpi/versions",
			Roles: []ocpi.CredentialsRole{{Role: ocpi.RoleCPO, PartyID: "OTH", CountryCode: "MY"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure(t, do(t, h, http.MethodPost, ocpi.CredentialsPath, "val-token-a", tc.body),
				http.StatusBadRequest, ocpi.StatusClientError)
		})
	}

	// A body that is not JSON at all is a 2000 as well.
	req := httptest.NewRequest(http.MethodPost, ocpi.CredentialsPath, strings.NewReader("{"))
	req.Header.Set("Authorization", "Token val-token-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	failure(t, rec, http.StatusBadRequest, ocpi.StatusClientError)

	// Nothing a rejected request did may be visible: the registration token is
	// still there and the partner is still pending.
	cpo, err := st.GetCPO(context.Background(), id)
	if err != nil {
		t.Fatalf("get cpo: %v", err)
	}
	if cpo.TokenA != "val-token-a" || cpo.TokenC != "" || cpo.Status != store.CPOStatusPending {
		t.Fatalf("a rejected request changed the partner: %+v", cpo)
	}
}

func TestPutCredentialsRotatesTokens(t *testing.T) {
	h, st := newServer(t)
	id := newFixtureCPO(t, st, "TS3", "", "rot-token-c")

	body := ocpi.Credentials{
		Token: "rotated-token-b",
		URL:   "http://fixture.test/ocpi/versions",
		Roles: []ocpi.CredentialsRole{{Role: ocpi.RoleCPO, PartyID: "TS3", CountryCode: "MY"}},
	}
	creds := success[ocpi.Credentials](t, do(t, h, http.MethodPut, ocpi.CredentialsPath, "rot-token-c", body))
	if creds.Token == "rot-token-c" || len(creds.Token) != 64 {
		t.Fatalf("Token C = %q, want a fresh 64-character token", creds.Token)
	}

	// The rotated-away Token C is dead; the new one works.
	failure(t, do(t, h, http.MethodGet, ocpi.CredentialsPath, "rot-token-c", nil),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)
	success[ocpi.Credentials](t, do(t, h, http.MethodGet, ocpi.CredentialsPath, creds.Token, nil))

	cpo, err := st.GetCPO(context.Background(), id)
	if err != nil {
		t.Fatalf("get cpo: %v", err)
	}
	if cpo.TokenB != "rotated-token-b" || cpo.TokenC != creds.Token {
		t.Fatalf("tokens not rotated: token_b=%q token_c=%q", cpo.TokenB, cpo.TokenC)
	}
	if want := "http://fixture.test/ocpi"; cpo.BaseURL != want {
		t.Fatalf("base_url = %q, want %q", cpo.BaseURL, want)
	}
	if cpo.HandshakeStatus != store.HandshakeInProgress {
		t.Fatalf("handshake_status = %q, want %q", cpo.HandshakeStatus, store.HandshakeInProgress)
	}
	if cpo.Status != store.CPOStatusActive {
		t.Fatalf("status = %q, want the partner to stay %q", cpo.Status, store.CPOStatusActive)
	}
}

func TestDeleteCredentialsUnregistersPartner(t *testing.T) {
	h, st := newServer(t)
	id := newFixtureCPO(t, st, "TS4", "", "del-token-c")

	success[map[string]any](t, do(t, h, http.MethodDelete, ocpi.CredentialsPath, "del-token-c", nil))

	// Both directions of the relationship are gone: the partner can neither call
	// ChargeNet nor be called by it.
	failure(t, do(t, h, http.MethodGet, ocpi.CredentialsPath, "del-token-c", nil),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)
	failure(t, do(t, h, http.MethodDelete, ocpi.CredentialsPath, "del-token-c", nil),
		http.StatusUnauthorized, ocpi.StatusUnauthorized)

	cpo, err := st.GetCPO(context.Background(), id)
	if err != nil {
		t.Fatalf("get cpo: %v", err)
	}
	if cpo.Status != store.CPOStatusSuspended {
		t.Fatalf("status = %q, want %q", cpo.Status, store.CPOStatusSuspended)
	}
	if cpo.TokenB != "" || cpo.TokenC != "" {
		t.Fatalf("tokens survived unregistration: token_b=%q token_c=%q", cpo.TokenB, cpo.TokenC)
	}
	if cpo.HandshakeStatus != store.HandshakeNotStarted {
		t.Fatalf("handshake_status = %q, want %q", cpo.HandshakeStatus, store.HandshakeNotStarted)
	}
}

// TestSuspendedPartnerLeavesDiscovery checks the driver-visible half of
// unregistering end to end: the suspended partner's stations disappear from
// `GET /api/v1/stations`, which filters discovery on the partner status.
func TestSuspendedPartnerLeavesDiscovery(t *testing.T) {
	st := testutil.NewTestStore(t)
	h := api.NewRouter(config.Config{
		JWTSecret:       "test-secret",
		MinStartBalance: 10,
		PublicBaseURL:   publicBaseURL,
		PlatformFeePct:  5,
	}, st)

	cpoID := newFixtureCPO(t, st, "TS5", "", "disc-token-c")
	stationID := newFixtureStation(t, st, cpoID)

	token := registerDriver(t, h)
	if !stationVisible(t, h, token, stationID) {
		t.Fatalf("fixture station %s is missing from discovery before unregistering", stationID)
	}

	success[map[string]any](t, do(t, h, http.MethodDelete, ocpi.CredentialsPath, "disc-token-c", nil))

	if stationVisible(t, h, token, stationID) {
		t.Fatalf("station %s is still offered after its partner was suspended", stationID)
	}
}

// registerDriver creates a driver through the mobile API and returns its token.
func registerDriver(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := bearer(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Ocpi Tester", "email": "ocpi-tester@example.com", "password": "Secret123",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register driver: status %d (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode register response %q: %v", rec.Body.String(), err)
	}
	if body.Token == "" {
		t.Fatalf("register response carried no token: %s", rec.Body)
	}
	return body.Token
}

// stationVisible reports whether discovery offers the given station id.
func stationVisible(t *testing.T, h http.Handler, token, id string) bool {
	t.Helper()
	rec := bearer(t, h, http.MethodGet, "/api/v1/stations", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list stations: status %d (%s)", rec.Code, rec.Body)
	}
	var stations []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stations); err != nil {
		t.Fatalf("decode stations %q: %v", rec.Body.String(), err)
	}
	for _, s := range stations {
		if s.ID == id {
			return true
		}
	}
	return false
}

// newFixtureStation gives a fixture partner one charge point so discovery has
// something to hide. Its cleanup is registered after the partner's, and cleanups
// run last-in-first-out, so the station goes before the cpo_partners row it
// references.
func newFixtureStation(t *testing.T, st *store.Store, cpoID string) string {
	t.Helper()
	id := cpoID + "_stn"
	clean := func() {
		if _, err := st.DB.ExecContext(context.Background(), `DELETE FROM stations WHERE id = $1`, id); err != nil {
			t.Logf("clean fixture station: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	if _, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO stations (id, cpo_id, name, address, city, latitude, longitude, total_bays, available_bays)
		 VALUES ($1, $2, 'Fixture Station', '1 Jalan Fixture', 'Kuala Lumpur', 3.100000, 101.600000, 2, 2)`,
		id, cpoID); err != nil {
		t.Fatalf("insert fixture station: %v", err)
	}
	// connectors cascade away with their station.
	if _, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO connectors (id, station_id, evse_uid, standard, format, power_type, max_power_kw, tariff_per_kwh)
		 VALUES ($1, $2, $3, 'CCS2', 'CABLE', 'DC', 60.00, 1.20)`,
		id+"_c1", id, "MY*TS5*E1"); err != nil {
		t.Fatalf("insert fixture connector: %v", err)
	}
	return id
}

// TestStoredCDRDecodesIntoTypes keeps the OCPI document the session manager writes and the types
// OCPI credentials defines from drifting apart: a stored CDR has to decode into [ocpi.CDR]
// with every field ChargeNet relies on.
func TestStoredCDRDecodesIntoTypes(t *testing.T) {
	start := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	raw, err := sessions.BuildCDR(store.CDRRequest{
		Session: &store.Session{ID: "csn_test", UserID: "usr_test"},
		Connector: store.Connector{
			ID: "con_test", StationID: "stn_test", EVSEUID: "MY*TS6*E1",
			Standard: store.StandardCCS2, Format: "CABLE", PowerType: "DC", MaxPowerKW: 60,
		},
		TariffID: "tar_test",
		Station: store.CDRStation{
			ID: "stn_test", Name: "Fixture Station", Address: "1 Jalan Fixture",
			City: "Kuala Lumpur", Latitude: 3.1, Longitude: 101.6,
		},
		CPOCountryCode: "MY",
		CPOPartyID:     "TS6",
		Amounts: store.SettleAmounts{
			CDRID: "cdr_test", Currency: "MYR", EnergyKWh: 12.5, DurationSeconds: 1800,
			TariffPerKWh: 1.2, Gross: 15, PlatformFeePct: 5, PlatformFee: 0.75, NetPayout: 14.25,
			StartTime: start, EndTime: start.Add(30 * time.Minute),
		},
	})
	if err != nil {
		t.Fatalf("build cdr: %v", err)
	}

	var cdr ocpi.CDR
	if err := json.Unmarshal(raw, &cdr); err != nil {
		t.Fatalf("decode stored cdr %s: %v", raw, err)
	}

	if cdr.CountryCode != "MY" || cdr.PartyID != "TS6" || cdr.ID != "cdr_test" || cdr.SessionID != "csn_test" {
		t.Fatalf("cdr identity = %+v", cdr)
	}
	if cdr.StartDateTime != start.Format(time.RFC3339) {
		t.Fatalf("start_date_time = %q", cdr.StartDateTime)
	}
	if cdr.AuthMethod != ocpi.AuthMethodCommand || cdr.Currency != "MYR" {
		t.Fatalf("auth_method/currency = %q/%q", cdr.AuthMethod, cdr.Currency)
	}
	if cdr.CDRToken.UID != "usr_test" || cdr.CDRToken.Type != "APP_USER" {
		t.Fatalf("cdr_token = %+v", cdr.CDRToken)
	}

	loc := cdr.CDRLocation
	if loc.ID != "stn_test" || loc.City != "Kuala Lumpur" || loc.ConnectorID != "con_test" {
		t.Fatalf("cdr_location = %+v", loc)
	}
	if loc.ConnectorStandard != "IEC_62196_T2_COMBO" || loc.ConnectorPowerType != "DC" {
		t.Fatalf("cdr_location connector = %+v", loc)
	}
	if loc.Coordinates.Latitude != "3.100000" {
		t.Fatalf("coordinates.latitude = %q, want a decimal string", loc.Coordinates.Latitude)
	}

	if len(cdr.Tariffs) != 1 || cdr.Tariffs[0].ID != "tar_test" || cdr.Tariffs[0].Currency != "MYR" {
		t.Fatalf("tariffs = %+v", cdr.Tariffs)
	}
	components := cdr.Tariffs[0].Elements[0].PriceComponents
	if len(components) != 1 || components[0].Type != "ENERGY" || components[0].Price != 1.2 || components[0].StepSize != 1 {
		t.Fatalf("price_components = %+v", components)
	}
	if len(cdr.ChargingPeriods) != 1 || len(cdr.ChargingPeriods[0].Dimensions) != 2 {
		t.Fatalf("charging_periods = %+v", cdr.ChargingPeriods)
	}
	if cdr.TotalEnergy != 12.5 || cdr.TotalTime != 0.5 || cdr.TotalCost.BeforeTaxes != 15 {
		t.Fatalf("totals = %v/%v/%v", cdr.TotalEnergy, cdr.TotalTime, cdr.TotalCost.BeforeTaxes)
	}
	if cdr.ChargeNet.Gross != 0 {
		t.Fatal("internal revenue leaked into OCPI document")
	}

}

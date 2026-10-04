package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// The portal of the API contract: cookie login, self-registration,
// tenancy, the handshake test and the super-admin console. These are the
// integration tests backend verification's "portal tenancy isolation" line asks for.

// portalRouter builds a router whose config carries a public base URL, so the
// cookie helpers exercise the same code path production uses.
func portalRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	cfg := config.Config{
		JWTSecret:       "test-secret",
		MinStartBalance: 10,
		PlatformFeePct:  5,
		PublicBaseURL:   "http://localhost:8080",
	}
	return api.NewRouter(cfg, st), st
}

// doCookie sends a request carrying a portal session cookie.
func doCookie(t *testing.T, h http.Handler, method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionCookie pulls the portal session cookie out of a response.
func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "cn_portal" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no cn_portal cookie in response: %v", rec.Result().Cookies())
	return nil
}

// purgePortalCPO removes a self-registered partner and its login, so a rerun
// starts from the same state.
func purgePortalCPO(t *testing.T, st *store.Store, cpoID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM portal_users WHERE cpo_id = $1`,
			`DELETE FROM cpo_partners WHERE id = $1`,
		} {
			if _, err := st.DB.ExecContext(ctx, q, cpoID); err != nil {
				t.Errorf("purge portal CPO %s: %v", cpoID, err)
			}
		}
	})
}

// registeredCPO is what the register response carries back.
type registeredCPO struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
		CPOID string `json:"cpoId"`
	} `json:"user"`
	CPO struct {
		ID         string  `json:"id"`
		Status     string  `json:"status"`
		PartyID    string  `json:"partyId"`
		TokenA     string  `json:"tokenA"`
		TokenBSet  bool    `json:"tokenBSet"`
		BankAccNo  string  `json:"bankAccountNo"`
		PayoutCyl  string  `json:"payoutCycle"`
		Handshake  string  `json:"handshakeStatus"`
		FeePercent float64 `json:"platformFeePct"`
	} `json:"cpo"`
}

// registerPortalCPO self-registers a partner and returns its session cookie and
// the created partner id.
func registerPortalCPO(t *testing.T, h http.Handler, email, company, party string) (*http.Cookie, string) {
	t.Helper()
	rec := doCookie(t, h, http.MethodPost, "/admin/api/register", nil, map[string]any{
		"companyName":  company,
		"partyId":      party,
		"countryCode":  "MY",
		"contactEmail": email,
		"contactName":  "Ops Lead",
		"password":     "Portal123",
		"bank": map[string]string{
			"bankName": "Maybank", "accountHolder": company, "accountNo": "1234567890",
		},
		"payoutCycle": "MONTHLY",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s = %d, want 201 (%s)", email, rec.Code, rec.Body)
	}
	out := decode[registeredCPO](t, rec)
	if out.CPO.ID == "" || out.CPO.TokenA == "" {
		t.Fatalf("register response missing cpo id/token A: %+v", out)
	}
	if out.CPO.Status != store.CPOStatusPending || out.CPO.Handshake != store.HandshakeNotStarted {
		t.Fatalf("new partner = %s/%s, want PENDING/NOT_STARTED", out.CPO.Status, out.CPO.Handshake)
	}
	return sessionCookie(t, rec), out.CPO.ID
}

// portalProfile is the flat shape `GET /admin/api/cpo` returns.
type portalProfile struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	TokenA        string  `json:"tokenA"`
	TokenBSet     bool    `json:"tokenBSet"`
	TokenCSet     bool    `json:"tokenCSet"`
	TokenBLast4   string  `json:"tokenBLast4"`
	TokenCLast4   string  `json:"tokenCLast4"`
	BankAccountNo string  `json:"bankAccountNo"`
	Handshake     string  `json:"handshakeStatus"`
	PlatformFee   float64 `json:"platformFeePct"`
}

// portalLogin signs a portal user in and returns the session cookie.
func portalLogin(t *testing.T, h http.Handler, email, password string) *http.Cookie {
	t.Helper()
	rec := doCookie(t, h, http.MethodPost, "/admin/api/login", nil, map[string]string{
		"email": email, "password": password,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("portal login %s = %d, want 200 (%s)", email, rec.Code, rec.Body)
	}
	return sessionCookie(t, rec)
}

// purgeSeedPortalUsers removes the four seeded logins so the suite leaves the
// shared database exactly as it found it.
func purgeSeedPortalUsers(t *testing.T, st *store.Store) {
	t.Helper()
	t.Cleanup(func() {
		_, err := st.DB.ExecContext(context.Background(),
			`DELETE FROM portal_users WHERE email = ANY($1)`,
			[]string{store.PortalAdminEmail, "ops@voltgo.my", "ops@chargesync.my", "ops@ecowatt.my"})
		if err != nil {
			t.Errorf("purge seed portal users: %v", err)
		}
	})
}

// TestPortalRegisterProfileAndHandshake walks the portal steps 3-5: register, read the
// profile back (bank number masked), rotate Token A, and run a handshake test
// that fails cleanly against a partner serving nothing.
func TestPortalRegisterProfileAndHandshake(t *testing.T) {
	h, st := portalRouter(t)
	cookie, cpoID := registerPortalCPO(t, h, "ops@acme.test", "Acme Charging", "ACM")
	purgePortalCPO(t, st, cpoID)

	rec := doCookie(t, h, http.MethodGet, "/admin/api/cpo", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/cpo = %d (%s)", rec.Code, rec.Body)
	}
	profile := decode[portalProfile](t, rec)
	if want := strings.Repeat("•", 6) + "7890"; profile.BankAccountNo != want {
		t.Fatalf("bankAccountNo = %q, want masked %q", profile.BankAccountNo, want)
	}
	if profile.TokenA == "" {
		t.Fatal("profile did not carry Token A for a partner that has not integrated yet")
	}

	rec = doCookie(t, h, http.MethodPost, "/admin/api/cpo/token-a/regenerate", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate Token A = %d (%s)", rec.Code, rec.Body)
	}
	rotated := decode[struct {
		TokenA string `json:"tokenA"`
	}](t, rec)
	if rotated.TokenA == "" || rotated.TokenA == profile.TokenA {
		t.Fatalf("rotated Token A = %q, want a new token", rotated.TokenA)
	}

	// No base URL, so step 1 (credentials) fails and the run is FAILED.
	rec = doCookie(t, h, http.MethodPost, "/admin/api/cpo/handshake-test", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("handshake-test = %d (%s)", rec.Code, rec.Body)
	}
	run := decode[struct {
		Status string `json:"status"`
		Steps  []struct {
			Step   int    `json:"step"`
			Label  string `json:"label"`
			Status string `json:"status"`
		} `json:"steps"`
	}](t, rec)
	if run.Status != store.HandshakeFailed {
		t.Fatalf("handshake status = %q, want FAILED (%+v)", run.Status, run.Steps)
	}
	if len(run.Steps) != 6 || run.Steps[0].Status != "FAIL" {
		t.Fatalf("handshake steps = %+v, want 6 starting with a FAIL", run.Steps)
	}

	// The outcome is persisted on the partner row.
	rec = doCookie(t, h, http.MethodGet, "/admin/api/cpo", cookie, nil)
	if got := decode[portalProfile](t, rec).Handshake; got != store.HandshakeFailed {
		t.Fatalf("stored handshakeStatus = %q, want FAILED", got)
	}
}

// TestPortalLoginRateLimited: five failures answer 401, the sixth answers 429.
func TestPortalLoginRateLimited(t *testing.T) {
	h, st := portalRouter(t)
	_, cpoID := registerPortalCPO(t, h, "ops@limit.test", "Limit Co", "LIM")
	purgePortalCPO(t, st, cpoID)

	for i := 1; i <= 5; i++ {
		rec := doCookie(t, h, http.MethodPost, "/admin/api/login", nil, map[string]string{
			"email": "ops@limit.test", "password": "wrong-password",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	rec := doCookie(t, h, http.MethodPost, "/admin/api/login", nil, map[string]string{
		"email": "ops@limit.test", "password": "Portal123",
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt = %d, want 429 (%s)", rec.Code, rec.Body)
	}
}

// TestPortalRequiresSession: an unauthenticated /admin/api call is refused.
func TestPortalRequiresSession(t *testing.T) {
	h, _ := portalRouter(t)
	rec := doCookie(t, h, http.MethodGet, "/admin/api/cpo", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d, want 401", rec.Code)
	}
	rec = doCookie(t, h, http.MethodGet, "/admin/api/cpo", &http.Cookie{Name: "cn_portal", Value: "not-a-jwt"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie = %d, want 401", rec.Code)
	}
}

// TestPortalAdminConsoleAndTenancy: a CPO-role user cannot reach the super-admin
// console; the super admin can list and suspend a partner, and
// the seeded CPO's own console can read its network and ledger.
func TestPortalAdminConsoleAndTenancy(t *testing.T) {
	h, st := portalRouter(t)
	ctx := context.Background()
	if err := st.SeedPortalUsers(ctx); err != nil {
		t.Fatalf("SeedPortalUsers: %v", err)
	}
	purgeSeedPortalUsers(t, st)

	// A CPO-role login is refused on an admin route.
	cpoCookie := portalLogin(t, h, "ops@voltgo.my", store.PortalCPOPassword)
	rec := doCookie(t, h, http.MethodGet, "/admin/api/admin/cpos", cpoCookie, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("CPO user on /admin/api/admin/cpos = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	// The super admin sees every partner.
	adminCookie := portalLogin(t, h, store.PortalAdminEmail, store.PortalAdminPassword)
	rec = doCookie(t, h, http.MethodGet, "/admin/api/admin/cpos", adminCookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin cpos = %d (%s)", rec.Code, rec.Body)
	}
	list := decode[struct {
		CPOs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"cpos"`
	}](t, rec)
	if len(list.CPOs) < 3 {
		t.Fatalf("admin sees %d partners, want at least the 3 seeded ones", len(list.CPOs))
	}

	// Suspend EcoWatt and edit its fee, then restore both (the shared DB must
	// not be left changed — it is not truncated between runs).
	fee := 5.0
	t.Cleanup(func() {
		if err := st.SetCPOStatus(context.Background(), "cpo_ecowatt", store.CPOStatusActive, &fee); err != nil {
			t.Errorf("restore EcoWatt: %v", err)
		}
	})
	suspend := 7.5
	rec = doCookie(t, h, http.MethodPut, "/admin/api/admin/cpos/cpo_ecowatt/status", adminCookie, map[string]any{
		"status": store.CPOStatusSuspended, "platformFeePct": suspend,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend EcoWatt = %d (%s)", rec.Code, rec.Body)
	}
	rec = doCookie(t, h, http.MethodGet, "/admin/api/admin/cpos", adminCookie, nil)
	after := decode[struct {
		CPOs []struct {
			ID     string  `json:"id"`
			Status string  `json:"status"`
			Fee    float64 `json:"platformFeePct"`
		} `json:"cpos"`
	}](t, rec)
	for _, row := range after.CPOs {
		if row.ID == "cpo_ecowatt" {
			if row.Status != store.CPOStatusSuspended || row.Fee != suspend {
				t.Fatalf("EcoWatt = %s @ %.2f, want SUSPENDED @ %.2f", row.Status, row.Fee, suspend)
			}
		}
	}

	// The CPO's own console reads its network, overview and ledger.
	rec = doCookie(t, h, http.MethodGet, "/admin/api/network", cpoCookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("network = %d (%s)", rec.Code, rec.Body)
	}
	net := decode[struct {
		Stations []struct {
			ID string `json:"id"`
		} `json:"stations"`
		Counts struct {
			Available int `json:"available"`
		} `json:"counts"`
	}](t, rec)
	if len(net.Stations) == 0 || net.Counts.Available == 0 {
		t.Fatalf("VoltGo network view = %d stations / %d available, want some", len(net.Stations), net.Counts.Available)
	}

	if rec = doCookie(t, h, http.MethodGet, "/admin/api/overview", cpoCookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body)
	}

	rec = doCookie(t, h, http.MethodGet, "/admin/api/cdrs", cpoCookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cdrs = %d (%s)", rec.Code, rec.Body)
	}

	rec = doCookie(t, h, http.MethodGet, "/admin/api/cdrs?format=csv", cpoCookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cdrs csv = %d (%s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("csv Content-Type = %q, want text/csv", ct)
	}
	if !strings.Contains(rec.Body.String(), "net_payout") {
		t.Fatalf("csv body is missing the header row: %q", rec.Body.String())
	}
}

// TestPortalDisconnectCredentials verifies credential disconnection: once a handshake has stored
// Tokens B/C, the console shows only their last four characters, and Disconnect
// clears both and returns the partner to the pre-handshake state.
func TestPortalDisconnectCredentials(t *testing.T) {
	h, st := portalRouter(t)
	cookie, cpoID := registerPortalCPO(t, h, "ops@disconnect.test", "Disconnect Co", "DSC")
	purgePortalCPO(t, st, cpoID)

	// Stand in for a completed OCPI handshake by storing Tokens B and C.
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE cpo_partners SET token_b = 'live_b_1234', token_c = 'live_c_5678',
		        handshake_status = 'VERIFIED' WHERE id = $1`, cpoID); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}

	before := decode[portalProfile](t, doCookie(t, h, http.MethodGet, "/admin/api/cpo", cookie, nil))
	if !before.TokenBSet || before.TokenBLast4 != "1234" {
		t.Fatalf("token B hint = set:%v last4:%q, want true/1234", before.TokenBSet, before.TokenBLast4)
	}
	if !before.TokenCSet || before.TokenCLast4 != "5678" {
		t.Fatalf("token C hint = set:%v last4:%q, want true/5678", before.TokenCSet, before.TokenCLast4)
	}

	rec := doCookie(t, h, http.MethodPost, "/admin/api/cpo/disconnect", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d (%s)", rec.Code, rec.Body)
	}

	after := decode[portalProfile](t, doCookie(t, h, http.MethodGet, "/admin/api/cpo", cookie, nil))
	if after.TokenBSet || after.TokenCSet {
		t.Fatalf("after disconnect token B/C = %v/%v, want both cleared", after.TokenBSet, after.TokenCSet)
	}
	if after.TokenA == "" {
		t.Fatal("disconnect must keep Token A (the partner stays registered)")
	}
	if after.Handshake != store.HandshakeNotStarted {
		t.Fatalf("handshake after disconnect = %s, want NOT_STARTED", after.Handshake)
	}
}

// TestPortalCDRDetailIsTenancyScopedAndFilterableByEVSE covers tenant CDR reads:
// a CDR is visible only to the partner that owns it, and the ledger's evse filter
// (used by the network drawer) is scoped to the caller the same way.
func TestPortalCDRDetailIsTenancyScopedAndFilterableByEVSE(t *testing.T) {
	h, st := portalRouter(t)
	ctx := context.Background()

	// A driver to attribute the CDR to — everything but the sandbox driver is
	// reported in the ledger.
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO users (id, name, email, password_hash) VALUES ('usr_cdrtest', 'CDR Test', 'cdrtest@test.local', 'x')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// A connector of the seeded VoltGo partner to hang a completed session on.
	var stationID, connectorID, evseUID string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT s.id, c.id, c.evse_uid FROM connectors c JOIN stations s ON s.id = c.station_id
		  WHERE s.cpo_id = 'cpo_voltgo' LIMIT 1`).Scan(&stationID, &connectorID, &evseUID); err != nil {
		t.Fatalf("find voltgo connector: %v", err)
	}

	const sessionID, cdrID = "sess_cdrtest", "cdr_cdrtest"
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO charging_sessions
		   (id, user_id, cpo_id, station_id, connector_id, evse_uid, status, start_time, end_time,
		    duration_seconds, tariff_per_kwh, total_cost)
		 VALUES ($1, 'usr_cdrtest', 'cpo_voltgo', $2, $3, $4, 'COMPLETED',
		         now() - interval '2 hours', now() - interval '1 hour', 3600, 0.60, 36.00)`,
		sessionID, stationID, connectorID, evseUID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO cdrs (id, session_id, cpo_id, user_id, total_energy_kwh, total_time_seconds,
		        gross_amount, platform_fee, net_payout, currency, cdr_json)
		 VALUES ($1, $2, 'cpo_voltgo', 'usr_cdrtest', 60.000, 3600, 36.00, 1.80, 34.20, 'MYR', '{"id":"cdr_cdrtest"}')`,
		cdrID, sessionID); err != nil {
		t.Fatalf("seed cdr: %v", err)
	}

	if err := st.SeedPortalUsers(ctx); err != nil {
		t.Fatalf("SeedPortalUsers: %v", err)
	}
	purgeSeedPortalUsers(t, st)

	voltgo := portalLogin(t, h, "ops@voltgo.my", store.PortalCPOPassword)
	foreign := portalLogin(t, h, "ops@chargesync.my", store.PortalCPOPassword)

	// The owner reads its own CDR.
	if rec := doCookie(t, h, http.MethodGet, "/admin/api/cdrs/"+cdrID, voltgo, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner CDR detail = %d (%s)", rec.Code, rec.Body)
	}
	// Another partner gets 404, not 403 — it must not learn the id exists.
	if rec := doCookie(t, h, http.MethodGet, "/admin/api/cdrs/"+cdrID, foreign, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign CDR detail = %d, want 404 (%s)", rec.Code, rec.Body)
	}

	// evse filter: the owner's drawer sees the bay's session; a foreign partner
	// sees none of it.
	ledger := decode[struct {
		Rows []struct {
			ID string `json:"id"`
		} `json:"rows"`
	}](t, doCookie(t, h, http.MethodGet, "/admin/api/cdrs?evse="+evseUID, voltgo, nil))
	if len(ledger.Rows) != 1 || ledger.Rows[0].ID != cdrID {
		t.Fatalf("owner evse-filtered ledger = %+v, want just %s", ledger.Rows, cdrID)
	}
	nothing := decode[struct {
		Rows []struct{} `json:"rows"`
	}](t, doCookie(t, h, http.MethodGet, "/admin/api/cdrs?evse="+evseUID, foreign, nil))
	if len(nothing.Rows) != 0 {
		t.Fatalf("foreign evse-filtered ledger = %d rows, want 0", len(nothing.Rows))
	}
}

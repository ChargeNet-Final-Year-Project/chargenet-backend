package api_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/auth"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/ocpi/emulator"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// This uses the mobile REST handlers and real loopback OCPI HTTP. No charging,
// conversion, telemetry or settlement business API is mocked.
func TestRoamingAcrossEveryEmulatorAndCountry(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	httpServer := httptest.NewUnstartedServer(nil)
	base := "http://" + httpServer.Listener.Addr().String()
	cfg := config.Config{PublicBaseURL: base, Gateway: "ocpi", JWTSecret: "roaming-test", MinStartBalance: 10, PlatformFeePct: 5, SimTimeScale: 60}
	app := api.NewServer(cfg, st)
	app.Sessions = sessions.NewManager(st, app.Gateway, cfg, sessions.WithTelemetryInterval(10*time.Millisecond))
	for i, old := range app.Emulators {
		old.Close()
		app.Emulators[i] = emulator.New(cfg, st, emulator.Profiles[i], emulator.WithTick(20*time.Millisecond), emulator.WithHeartbeat(0))
	}
	handler := app.Handler()
	httpServer.Config.Handler = handler
	httpServer.Start()
	t.Cleanup(func() { cancel(); app.Close(); httpServer.Close() })
	for _, profile := range emulator.Profiles {
		cpo, err := st.GetCPO(ctx, profile.CPOID)
		if err != nil {
			t.Fatal(err)
		}
		oldURL, oldVersion := cpo.BaseURL, cpo.OCPIVersion
		t.Cleanup(func() {
			st.DB.Exec(`UPDATE cpo_partners SET base_url=$2,ocpi_version=$3 WHERE id=$1`, profile.CPOID, oldURL, oldVersion)
		})
		if _, err = st.DB.Exec(`UPDATE cpo_partners SET base_url=$2,ocpi_version='2.3.0' WHERE id=$1`, profile.CPOID, base+"/emulator/"+profile.Slug+"/ocpi"); err != nil {
			t.Fatal(err)
		}
	}
	app.Run(ctx)
	if err := app.Sync.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(ctx, "Cross border driver", "roaming@example.test", "hash", "MYR")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TopUpWallet(ctx, user.ID, 1000, store.PaymentCreditCard); err != nil {
		t.Fatal(err)
	}
	token, _ := auth.IssueMobileToken(cfg.JWTSecret, user.ID)
	for _, profile := range emulator.Profiles {
		for _, country := range []string{"MY", "SG", "TH"} {
			t.Run(profile.Slug+"_"+country, func(t *testing.T) {
				var uid string
				err := st.DB.QueryRow(`SELECT k.evse_uid FROM connectors k JOIN stations s ON s.id=k.station_id WHERE s.cpo_id=$1 AND s.country_code=$2 AND s.id LIKE 'stn_demo_%' AND k.power_type='DC' AND k.status='AVAILABLE' ORDER BY s.id LIMIT 1`, profile.CPOID, country).Scan(&uid)
				if err != nil {
					t.Fatal(err)
				}
				before, _ := st.GetWallet(ctx, user.ID)
				qr := doJSON(t, handler, "POST", "/api/v1/quotes", token, map[string]any{"evseUid": uid, "estimatedKwh": "15"})
				if qr.Code != 201 {
					t.Fatalf("quote %d %s", qr.Code, qr.Body)
				}
				quote := decode[store.ChargingQuote](t, qr)
				startBody := map[string]any{"evseUid": uid, "quoteId": quote.ID, "targetSoc": 100}
				startHeaders := map[string]string{"Idempotency-Key": "roaming-start-" + uid}
				start := doJSONHeaders(t, handler, "POST", "/api/v1/sessions/start", token, startHeaders, startBody)
				if start.Code != 201 {
					t.Fatalf("start %d %s", start.Code, start.Body)
				}
				session := decode[store.Session](t, start)
				repeated := doJSONHeaders(t, handler, "POST", "/api/v1/sessions/start", token, startHeaders, startBody)
				if repeated.Code != 201 || repeated.Header().Get("Idempotent-Replay") != "true" || decode[store.Session](t, repeated).ID != session.ID {
					t.Fatalf("repeated start was not replayed: %d %s", repeated.Code, repeated.Body)
				}
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					row, e := st.SessionByID(ctx, session.ID)
					if e == nil && row.KWhDelivered > 0 {
						session = *row
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if session.KWhDelivered <= 0 {
					t.Fatal("no telemetry arrived")
				}
				stop := doJSON(t, handler, "POST", "/api/v1/sessions/"+session.ID+"/stop", token, nil)
				if stop.Code != 200 {
					t.Fatalf("stop %d %s", stop.Code, stop.Body)
				}
				done := decode[store.Session](t, stop)
				if !done.Terminal() || done.CDRID == nil || done.TotalCost <= 0 {
					t.Fatalf("unsettled %+v", done)
				}
				if done.CpoID != profile.CPOID || done.ExchangeRate != quote.ExchangeRate {
					t.Fatal("operator/FX changed")
				}
				after, _ := st.GetWallet(ctx, user.ID)
				if fmt.Sprintf("%.2f", before.Balance-after.Balance) != fmt.Sprintf("%.2f", done.TotalCost) || after.Reserved != 0 {
					t.Fatalf("wallet deduction mismatch %+v", after)
				}
				cdr, e := st.GetCDR(ctx, *done.CDRID)
				if e != nil {
					t.Fatal(e)
				}
				if cdr.Currency != quote.OriginalCurrency || cdr.GrossAmount != done.OriginalCost {
					t.Fatalf("CDR currency/cost %+v", cdr)
				}
				var count int
				st.DB.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE session_id=$1 AND cdr_id=$2`, done.ID, *done.CDRID).Scan(&count)
				if count != 1 {
					t.Fatalf("settlement transactions %d", count)
				}
				replay := doJSON(t, handler, "POST", "/api/v1/sessions/"+session.ID+"/stop", token, nil)
				if replay.Code != 200 {
					t.Fatal("duplicate stop failed")
				}
				t.Logf("%s %s: %.3f kWh, %s %.2f, rate %s, MYR %.2f deducted, CDR %s", profile.Slug, country, done.KWhDelivered, done.OriginalCurrency, done.OriginalCost, done.ExchangeRate, done.TotalCost, *done.CDRID)
			})
		}
	}
}

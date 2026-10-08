package api_test

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

func TestPortalWholeRangeCSVAndBusinessBoundary(t *testing.T) {
	for _, count := range []int{26, 630} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h, st := portalRouter(t)
			ctx := context.Background()
			for _, id := range []string{"usr_report", store.SandboxUserID} {
				if _, err := st.DB.ExecContext(ctx, `INSERT INTO users (id,name,email,password_hash) VALUES ($1::text,'Report',$1::text || '@test.local','x') ON CONFLICT (id) DO NOTHING`, id); err != nil {
					t.Fatal(err)
				}
			}
			var station, connector, evse string
			if err := st.DB.QueryRowContext(ctx, `SELECT s.id,k.id,k.evse_uid FROM connectors k JOIN stations s ON s.id=k.station_id WHERE s.cpo_id='cpo_voltgo' ORDER BY k.id LIMIT 1`).Scan(&station, &connector, &evse); err != nil {
				t.Fatal(err)
			}
			seed := func(prefix, user, cpo, at string, n int) {
				t.Helper()
				_, err := st.DB.ExecContext(ctx, `INSERT INTO charging_sessions
     (id,user_id,cpo_id,station_id,connector_id,evse_uid,status,start_time,end_time,duration_seconds,tariff_per_kwh,total_cost)
     SELECT $1 || lpad(i::text,4,'0'),$2,$3,$4,$5,$6,'COMPLETED',TIMESTAMPTZ '2026-09-01' + i * interval '1 second',TIMESTAMPTZ '2026-09-01' + i * interval '1 second' + interval '1 hour',3600,1.60,1.60 FROM generate_series(1,$7::int) i`, prefix, user, cpo, station, connector, evse, n)
				if err != nil {
					t.Fatal(err)
				}
				_, err = st.DB.ExecContext(ctx, `INSERT INTO cdrs
     (id,session_id,cpo_id,user_id,total_energy_kwh,total_time_seconds,gross_amount,platform_fee,net_payout,currency,cdr_json,created_at)
     SELECT 'cdr_' || $1 || lpad(i::text,4,'0'),$1 || lpad(i::text,4,'0'),$2,$3,1,3600,1.60,0.08,1.52,'MYR','{}',$4::timestamptz FROM generate_series(1,$5::int) i`, prefix, cpo, user, at, n)
				if err != nil {
					t.Fatal(err)
				}
			}
			seed("report_", "usr_report", "cpo_voltgo", "2026-10-01T16:00:00Z", count)
			seed("before_", "usr_report", "cpo_voltgo", "2026-10-01T15:59:59.999999Z", 1)
			seed("after_", "usr_report", "cpo_voltgo", "2026-10-02T16:00:00Z", 1)
			seed("sandbox_", store.SandboxUserID, "cpo_voltgo", "2026-10-01T17:00:00Z", 1)
			if err := st.SeedPortalUsers(ctx); err != nil {
				t.Fatal(err)
			}
			purgeSeedPortalUsers(t, st)
			own := portalLogin(t, h, "ops@voltgo.my", store.PortalCPOPassword)
			foreign := portalLogin(t, h, "ops@chargesync.my", store.PortalCPOPassword)
			path := "/admin/api/cdrs?from=2026-10-02&to=2026-10-02"
			type response struct {
				Rows     []store.LedgerRow  `json:"rows"`
				Totals   store.LedgerTotals `json:"totals"`
				Timezone string             `json:"reportingTimezone"`
			}
			page := decode[response](t, doCookie(t, h, http.MethodGet, path, own, nil))
			if len(page.Rows) != 25 || page.Totals.CDRs != count || page.Timezone != store.ReportingTimezone {
				t.Fatalf("wrong JSON page: %+v", page)
			}
			if page.Rows[0].CDRID != fmt.Sprintf("cdr_report_%04d", count) {
				t.Fatalf("unstable settlement order: %s", page.Rows[0].CDRID)
			}
			page2 := decode[response](t, doCookie(t, h, http.MethodGet, path+"&page=2", own, nil))
			if page2.Rows[0].CDRID != fmt.Sprintf("cdr_report_%04d", count-25) {
				t.Fatal("page boundary duplicated/reordered")
			}
			for _, tc := range []struct {
				query  string
				cookie *http.Cookie
				want   int
			}{
				{"&format=csv", own, count}, {"&format=csv&limit=1&page=99&evse=" + evse, own, count},
				{"&format=csv&evse=missing", own, 0}, {"&format=csv", foreign, 0},
			} {
				rec := doCookie(t, h, http.MethodGet, path+tc.query, tc.cookie, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("CSV = %d: %s", rec.Code, rec.Body)
				}
				rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != tc.want+2 || len(rows[0]) != 13 || rows[len(rows)-1][0] != "TOTAL" {
					t.Fatalf("CSV incomplete: %d rows, want %d", len(rows), tc.want+2)
				}
				if rows[len(rows)-1][9] != fmt.Sprintf("%.2f", float64(tc.want)*1.6) || rows[len(rows)-1][11] != fmt.Sprintf("%.2f", float64(tc.want)*1.52) {
					t.Fatal("CSV totals do not match exported population")
				}
				seen := map[string]bool{}
				for _, row := range rows[1 : len(rows)-1] {
					if !strings.HasPrefix(row[0], "cdr_report_") || seen[row[0]] {
						t.Fatalf("foreign/sandbox/duplicate/out-of-range row: %s", row[0])
					}
					seen[row[0]] = true
				}
			}
			from, _ := time.Parse(time.RFC3339, "2026-10-01T16:00:00Z")
			visited := 0
			totals, err := st.ExportCDRLedger(ctx, "cpo_voltgo", evse, from, from.Add(24*time.Hour), func(row store.LedgerRow) error {
				visited++
				if visited == 1 {
					seed("concurrent_", "usr_report", "cpo_voltgo", "2026-10-01T18:00:00Z", 1)
				}
				return nil
			})
			if err != nil || visited != count || totals.CDRs != count {
				t.Fatalf("export snapshot drifted during settlement: visited=%d totals=%d err=%v", visited, totals.CDRs, err)
			}
			cancelled := errors.New("cancel export")
			if _, err := st.ExportCDRLedger(ctx, "cpo_voltgo", evse, from, from.Add(24*time.Hour), func(store.LedgerRow) error { return cancelled }); !errors.Is(err, cancelled) {
				t.Fatalf("consumer failure was lost: %v", err)
			}

			for _, query := range []string{"?from=bad", "?from=2026-10-03&to=2026-10-02", "?from=2026-02-30"} {
				if rec := doCookie(t, h, http.MethodGet, "/admin/api/cdrs"+query, own, nil); rec.Code != 400 {
					t.Fatalf("invalid range returned %d", rec.Code)
				}
			}
		})
	}
}

func TestPortalDashboardBusinessDaysIgnoreDatabaseTimezone(t *testing.T) {
	_, st := portalRouter(t)
	ctx := context.Background()
	st.DB.SetMaxOpenConns(1)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO users(id,name,email,password_hash) VALUES ('usr_days','Days','days@test.local','x')`); err != nil {
		t.Fatal(err)
	}
	var station, connector, evse string
	if err := st.DB.QueryRowContext(ctx, `SELECT s.id,k.id,k.evse_uid FROM connectors k JOIN stations s ON s.id=k.station_id WHERE s.cpo_id='cpo_voltgo' LIMIT 1`).Scan(&station, &connector, &evse); err != nil {
		t.Fatal(err)
	}
	local := time.Now().In(store.ReportingLocation)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, store.ReportingLocation)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, store.ReportingLocation)
	timestamps := []time.Time{today.Add(-time.Microsecond), today, today.AddDate(0, 0, 1)}
	expectedMTD := 0.0
	for i, at := range timestamps {
		id := fmt.Sprintf("day_%d", i)
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO charging_sessions(id,user_id,cpo_id,station_id,connector_id,evse_uid,status,start_time,end_time,duration_seconds,tariff_per_kwh,total_cost)
    VALUES ($1,'usr_days','cpo_voltgo',$2,$3,$4,'COMPLETED',$5,$5,0,1.60,1.60)`, id, station, connector, evse, at); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO cdrs(id,session_id,cpo_id,user_id,total_energy_kwh,total_time_seconds,gross_amount,platform_fee,net_payout,currency,cdr_json,created_at)
    VALUES ($1,$1,'cpo_voltgo','usr_days',1,0,1.60,0.08,1.52,'MYR','{}',$2)`, id, at); err != nil {
			t.Fatal(err)
		}
		if !at.Before(month) && at.Before(today.AddDate(0, 0, 1)) {
			expectedMTD += 1.52
		}
	}
	for _, zone := range []string{"UTC", "America/Los_Angeles", "Pacific/Kiritimati"} {
		if _, err := st.DB.ExecContext(ctx, `SELECT set_config('TimeZone',$1,false)`, zone); err != nil {
			t.Fatal(err)
		}
		overview, err := st.OverviewForCPO(ctx, "cpo_voltgo")
		if err != nil {
			t.Fatal(err)
		}
		if overview.SessionsToday != 1 || overview.GrossToday != 1.6 || overview.NetPayoutMTD != expectedMTD {
			t.Fatalf("%s shifted overview: %+v", zone, overview)
		}
		if len(overview.Last7Days) != 7 || overview.Last7Days[6].Day != today.Format("2006-01-02") || overview.Last7Days[6].Gross != 1.6 {
			t.Fatalf("%s shifted chart: %+v", zone, overview.Last7Days)
		}
		admin, err := st.AdminOverview(ctx)
		if err != nil || admin.SessionsToday != 1 || admin.GrossToday != 1.6 {
			t.Fatalf("%s shifted admin: %+v %v", zone, admin, err)
		}
		network, err := st.NetworkForCPO(ctx, "cpo_voltgo")
		if err != nil || network.SessionsToday != 1 {
			t.Fatalf("%s shifted network: %+v %v", zone, network, err)
		}
		partners, err := st.ListAllCPOsAdmin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, partner := range partners {
			if partner.ID == "cpo_voltgo" && (partner.SessionsToday != 1 || partner.GrossToday != 1.6) {
				t.Fatalf("%s shifted partner: %+v", zone, partner)
			}
		}
	}
}

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
	"github.com/chargenet/chargenet-backend/scripts"
)

func TestUpgradePreservesLegacyFinancialHistory(t *testing.T) {
	admin := testutil.NewTestDB(t)
	ctx := context.Background()
	schema := store.NewID("migration_test")
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA %q CASCADE`, schema))
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://chargenet:chargenet@localhost:5432/chargenet_test?sslmode=disable"
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	legacy, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err = legacy.ExecContext(ctx, scripts.InitSQL); err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(ctx, `
 INSERT INTO users(id,name,email,password_hash) VALUES('legacy-driver','Historical driver','legacy@example.test','hash');
 INSERT INTO wallets(id,user_id,balance,currency) VALUES('legacy-wallet','legacy-driver',54.45,'MYR');
 INSERT INTO charging_sessions(id,user_id,cpo_id,station_id,connector_id,evse_uid,status,start_time,end_time,kwh_delivered,tariff_per_kwh,total_cost,balance_after)
 VALUES('legacy-session','legacy-driver','cpo_voltgo','stn_vlt_sgb','con_vlt_sgb_01','VLT-SGB-01','COMPLETED',now()-interval '1 hour',now(),20.7,1.5,31.05,54.45);
 INSERT INTO cdrs(id,session_id,cpo_id,user_id,total_energy_kwh,total_time_seconds,gross_amount,platform_fee,net_payout,currency,cdr_json)
 VALUES('legacy-cdr','legacy-session','cpo_voltgo','legacy-driver',20.7,3600,31.05,1.55,29.50,'MYR','{"historical":true}');
 UPDATE charging_sessions SET cdr_id='legacy-cdr' WHERE id='legacy-session';
 INSERT INTO stations(id,cpo_id,ocpi_location_id,name,address,city,latitude,longitude)
 VALUES('stn_demo_george_town','cpo_voltgo','DEMO-GEORGE-TOWN','ChargeNet Demo - George Town','Test site','George Town',5.419,100.331),
 ('stn_demo_gurney','cpo_chargesync','DEMO-GURNEY','ChargeSync Custom Name','Test site','Gurney Bay',5.43,100.31);
 INSERT INTO wallet_transactions(id,wallet_id,type,amount,payment_method,reference_id) VALUES('legacy-tx','legacy-wallet','CHARGING_DEBIT',31.05,'Wallet','legacy-cdr');`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.Migrate(ctx, legacy); err != nil {
			t.Fatalf("upgrade pass %d: %v", i, err)
		}
	}
	var intact bool
	err = legacy.QueryRowContext(ctx, `SELECT w.balance=54.45 AND w.currency='MYR' AND s.total_cost=31.05 AND s.original_cost=31.05
 AND s.quote_id IS NULL AND c.cdr_json='{"historical":true}'::jsonb AND t.session_id=s.id AND t.cdr_id=c.id
 FROM wallets w JOIN charging_sessions s ON s.user_id=w.user_id JOIN cdrs c ON c.session_id=s.id
 JOIN wallet_transactions t ON t.wallet_id=w.id WHERE w.id='legacy-wallet'`).Scan(&intact)
	if err != nil || !intact {
		t.Fatalf("legacy history changed: %v, %v", intact, err)
	}
	var renamed, custom string
	if err = legacy.QueryRow(`SELECT name FROM stations WHERE id='stn_demo_george_town'`).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if err = legacy.QueryRow(`SELECT name FROM stations WHERE id='stn_demo_gurney'`).Scan(&custom); err != nil {
		t.Fatal(err)
	}
	if renamed != "VoltGo George Town" || custom != "ChargeSync Custom Name" {
		t.Fatalf("seed upgrade names: %s, %s", renamed, custom)
	}
}

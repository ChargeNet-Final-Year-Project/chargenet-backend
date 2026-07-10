// Package testutil provides helpers shared by integration tests. Tests that
// need PostgreSQL skip themselves when no database is reachable, so `go test`
// still works on a machine without Docker.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/chargenet/chargenet-backend/internal/store"
)

const defaultTestDSN = "postgres://chargenet:chargenet@localhost:5432/chargenet_test?sslmode=disable"

// NewTestStore connects to the test database, creates it if missing, applies
// the schema and empties the driver tables. It returns a *store.Store and
// registers cleanup. If the database cannot be reached the test is skipped.
func NewTestStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	ensureDatabase(t, dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("testutil: no test database available (%v); skipping integration test", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("testutil: migrate: %v", err)
	}

	st := store.New(db)
	truncate(t, db)

	t.Cleanup(func() {
		truncate(t, db)
		_ = db.Close()
	})
	return st
}

// NewTestDB is the raw *sql.DB variant for tests that need to poke the schema.
func NewTestDB(t *testing.T) *sql.DB {
	return NewTestStore(t).DB
}

func ensureDatabase(t *testing.T, dsn string) {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Skipf("testutil: bad TEST_DATABASE_URL: %v", err)
	}
	dbName := u.Path[1:]
	if dbName == "" {
		t.Skip("testutil: TEST_DATABASE_URL has no database name")
	}
	if !strings.HasSuffix(dbName, "_test") && !strings.HasPrefix(dbName, "chargenet_test_") {
		t.Fatal("testutil: refusing destructive test setup outside a database named *_test or chargenet_test_*")
	}

	admin := *u
	admin.Path = "/postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := sql.Open("pgx", admin.String())
	if err != nil {
		t.Skipf("testutil: cannot open admin connection: %v", err)
	}
	defer conn.Close()
	if err := conn.PingContext(ctx); err != nil {
		t.Skipf("testutil: test database unreachable (%v); skipping", err)
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %q", dbName))
	if err != nil && !isAlreadyExists(err) {
		t.Fatalf("testutil: create database %q: %v", dbName, err)
	}
}

// truncate empties the mutable tables (users cascade to wallets, vehicles,
// sessions and cdrs). Seed data for CPOs/stations is left intact.
func truncate(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `TRUNCATE users, wallets, vehicles, charging_sessions, cdrs, idempotency_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("testutil: truncate: %v", err)
	}
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "42P04")
}

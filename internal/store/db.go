// Package store owns the PostgreSQL connection and every repository that
// reads or writes the ChargeNet schema. Money is handled in NUMERIC(10,2)
// columns; helpers here keep rounding consistent (see Round2).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/chargenet/chargenet-backend/scripts"
)

// Store wraps the database handle. Repository methods live on *Store so that
// handlers depend on a single, mockable value.
type Store struct {
	DB *sql.DB
}

// New wraps an existing *sql.DB.
func New(db *sql.DB) *Store {
	return &Store{DB: db}
}

// Open connects to PostgreSQL using the pgx stdlib driver and waits for the
// database to accept queries (10 attempts, 1 s apart) so `go run` can start
// immediately after `docker compose up -d`.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = db.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return db, nil
		}
		select {
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	_ = db.Close()
	return nil, lastErr
}

// Migrate applies the embedded schema/seed script. It is safe to run on every
// boot because every statement is idempotent.
func Migrate(ctx context.Context, db *sql.DB) error {
	if scripts.InitSQL == "" {
		return errors.New("store: embedded init.sql is empty")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(238701)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, scripts.InitSQL); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := scripts.Migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, entry.Name()).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		raw, e := scripts.Migrations.ReadFile("migrations/" + entry.Name())
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, string(raw)); e != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), e)
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, entry.Name()); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Health returns nil when the database answers a trivial query.
func Health(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Round2 rounds a ringgit amount to two decimals. All balances and costs are
// stored as NUMERIC(10,2); we never do float math on money, only round at the
// edges.
func Round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrEmailTaken is returned when a unique email constraint is violated.
var ErrEmailTaken = errors.New("store: email already taken")

// ErrInsufficientBalance is returned by DebitWallet when the wallet cannot
// cover the requested amount. It maps to the INSUFFICIENT_BALANCE API error.
var ErrInsufficientBalance = errors.New("store: insufficient balance")

// ErrCpoInactive is returned when a row belongs to a CPO partner that is not
// ACTIVE. Suspending a partner hides its stations from discovery, so
// an EVSE the driver can no longer see must not be chargeable through a scanned
// code either; the start path turns this into CONNECTOR_UNAVAILABLE.
var ErrCpoInactive = errors.New("store: cpo not active")

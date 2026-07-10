package store

import (
	"context"
	"fmt"
	"time"

	"github.com/chargenet/chargenet-backend/internal/auth"
)

// Seeded driver credentials. Exported so tests and the
// README can reference them.
const (
	DemoEmail    = "demo@chargenet.my"
	DemoPassword = "Demo1234!"
)

// SandboxUserID is the internal driver the portal sandbox bills to. Its
// sessions are excluded from CPO revenue reports.
const SandboxUserID = "usr_sandbox"

// SeedUsers inserts the demo driver (wallet RM 85.50 + default vehicle) and
// the sandbox driver (wallet RM 1000) the first time the backend boots. It is
// a no-op once any driver exists.
func (s *Store) SeedUsers(ctx context.Context) error {
	n, err := s.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	if err := s.seedDemoDriver(ctx); err != nil {
		return fmt.Errorf("seed demo driver: %w", err)
	}
	if err := s.seedSandboxDriver(ctx); err != nil {
		return fmt.Errorf("seed sandbox driver: %w", err)
	}
	return nil
}

func (s *Store) seedDemoDriver(ctx context.Context) error {
	hash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		return err
	}

	user := &User{
		ID:           NewID("usr"),
		Name:         "Mark Tan",
		Email:        DemoEmail,
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
	}
	walletID := NewID("wal")
	vehicleID := NewID("veh")

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, name, email, password_hash) VALUES ($1, $2, $3, $4)`,
		user.ID, user.Name, user.Email, user.PasswordHash,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wallets (id, user_id, balance, currency) VALUES ($1, $2, 85.50, 'MYR')`,
		walletID, user.ID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vehicles (id, user_id, name, license_plate, connector_type, battery_capacity, is_default)
		 VALUES ($1, $2, 'Tesla Model Y', 'PCN1234', 'CCS2', 75.00, TRUE)`,
		vehicleID, user.ID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) seedSandboxDriver(ctx context.Context) error {
	hash, err := auth.HashPassword(NewID("pw"))
	if err != nil {
		return err
	}
	walletID := NewID("wal")

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, name, email, password_hash) VALUES ($1, 'Sandbox Driver', 'sandbox@chargenet.internal', $2)`,
		SandboxUserID, hash,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wallets (id, user_id, balance, currency) VALUES ($1, $2, 1000.00, 'MYR')`,
		walletID, SandboxUserID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/chargenet/chargenet-backend/internal/auth"
)

// Seeded portal credentials. Exported so tests, the README
// and the demo script can reference them; passwords are hashed before storage.
const (
	PortalAdminEmail    = "admin@chargenet.my"
	PortalAdminPassword = "Admin1234!"
	PortalCPOPassword   = "Cpo1234!"
)

// portalSeedUsers pairs each seeded partner's slug with the portal login it
// gets, in a fixed order so a fresh database seeds deterministically.
var portalSeedUsers = []struct{ slug, email string }{
	{"voltgo", "ops@voltgo.my"},
	{"chargesync", "ops@chargesync.my"},
	{"ecowatt", "ops@ecowatt.my"},
}

// SeedPortalUsers inserts the super admin and one CPO-role login per seeded
// partner the first time the backend boots. It is a no-op once any portal login
// exists, mirroring [Store.SeedUsers].
//
// Missing partner slugs are skipped so older databases still start. The CPO can
// register again through the portal.
func (s *Store) SeedPortalUsers(ctx context.Context) error {
	n, err := s.CountPortalUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	adminHash, err := auth.HashPassword(PortalAdminPassword)
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO portal_users (id, email, password_hash, name, role, cpo_id)
		 VALUES ($1, $2, $3, 'ChargeNet Admin', $4, NULL)`,
		NewID("pu"), PortalAdminEmail, adminHash, PortalRoleSuperAdmin); err != nil {
		return fmt.Errorf("seed portal admin: %w", err)
	}

	cpoHash, err := auth.HashPassword(PortalCPOPassword)
	if err != nil {
		return err
	}
	for _, u := range portalSeedUsers {
		var cpoID string
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM cpo_partners WHERE slug = $1`, u.slug).Scan(&cpoID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("seed portal user %s: %w", u.email, err)
		}
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO portal_users (id, email, password_hash, name, role, cpo_id)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			NewID("pu"), u.email, cpoHash, "Ops "+u.slug, PortalRoleCPO, cpoID); err != nil {
			return fmt.Errorf("seed portal user %s: %w", u.email, err)
		}
	}
	return nil
}

package store

import (
	"context"
	"net/url"
	"strings"
)

// ConfigureDemoNetworks follows PUBLIC_BASE_URL only for the shipped loopback
// emulators with their known development credentials. External CPO endpoints
// and operator-managed credentials are preserved.
func (s *Store) ConfigureDemoNetworks(ctx context.Context, base string) error {
	for _, slug := range []string{"voltgo", "chargesync", "ecowatt"} {
		var current string
		if err := s.DB.QueryRowContext(ctx, `SELECT base_url FROM cpo_partners WHERE id=$1`, "cpo_"+slug).Scan(&current); err != nil {
			continue
		}
		u, err := url.Parse(current)
		if err != nil {
			continue
		}
		if (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Path != "/emulator/"+slug+"/ocpi" {
			continue
		}
		if _, err = s.DB.ExecContext(ctx, `UPDATE cpo_partners SET base_url=$2 WHERE id=$1 AND token_b=$3 AND token_c=$4`, "cpo_"+slug, strings.TrimRight(base, "/")+u.Path, slug+"-token-b-dev", slug+"-token-c-dev"); err != nil {
			return err
		}
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CPO partner status and OCPI handshake status values.
const (
	CPOStatusPending   = "PENDING"
	CPOStatusActive    = "ACTIVE"
	CPOStatusSuspended = "SUSPENDED"

	HandshakeNotStarted = "NOT_STARTED"
	HandshakeInProgress = "IN_PROGRESS"
	HandshakeVerified   = "VERIFIED"
	HandshakeFailed     = "FAILED"
)

// CpoAccount is a CPO partner row including its OCPI credentials. Unlike
// [CpoPartner] (the trimmed DTO the driver app sees) it carries the token
// columns, so it never leaves the backend: only the OCPI layer, the
// handshake test and the portal may hold one.
type CpoAccount struct {
	ModuleEndpoints map[string]string
	ID              string
	Name            string
	Slug            string
	CountryCode     string
	PartyID         string
	BaseURL         string
	ColorHex        string
	ContactEmail    string
	// TokenA is the one-time token ChargeNet hands the CPO; empty once the
	// credential exchange consumed it.
	TokenA       string
	TokenAUsedAt *time.Time
	// TokenB is what ChargeNet sends to the CPO, TokenC what the CPO sends
	// back to ChargeNet.
	TokenB          string
	TokenC          string
	OCPIVersion     string
	HandshakeStatus string
	PlatformFeePct  float64
	Status          string
	CreatedAt       time.Time
}

// cpoColumns is the select list [scanCPO] expects, so every query below scans
// into the same fields in the same order.
const cpoColumns = `id, name, slug, country_code, party_id, base_url, color_hex,
	contact_email, token_a, token_a_used_at, token_b, token_c, ocpi_version,
	handshake_status, platform_fee_pct, status, created_at,module_endpoints`

// GetCPO reads one CPO partner by id.
func (s *Store) GetCPO(ctx context.Context, id string) (*CpoAccount, error) {
	return scanCPO(s.DB.QueryRowContext(ctx,
		`SELECT `+cpoColumns+` FROM cpo_partners WHERE id = $1`, id))
}

// GetCPOByTokenA resolves the one-time registration token.
// A suspended partner cannot re-register with a leftover token.
func (s *Store) GetCPOByTokenA(ctx context.Context, token string) (*CpoAccount, error) {
	if strings.TrimSpace(token) == "" {
		// Guards against matching a row whose token column is NULL/empty.
		return nil, ErrNotFound
	}
	return scanCPO(s.DB.QueryRowContext(ctx,
		`SELECT `+cpoColumns+` FROM cpo_partners
		  WHERE token_a = $1 AND status <> 'SUSPENDED'`, token))
}

// GetCPOByTokenC resolves the operational token a registered CPO calls
// ChargeNet with. Only ACTIVE partners authenticate, so suspending a CPO also
// silences its pushes and command callbacks.
func (s *Store) GetCPOByTokenC(ctx context.Context, token string) (*CpoAccount, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNotFound
	}
	return scanCPO(s.DB.QueryRowContext(ctx,
		`SELECT `+cpoColumns+` FROM cpo_partners
		  WHERE token_c = $1 AND status = 'ACTIVE'`, token))
}

// CompleteHandshake stores what the CPO sent in `POST /credentials` (Token B and
// its versions root) together with the fresh Token C ChargeNet replies with.
// Token A is invalidated in the same statement, which is what makes a second use
// of it fail. The handshake stays IN_PROGRESS until the
// verification in the portal promotes it to VERIFIED.
func (s *Store) CompleteHandshake(ctx context.Context, cpoID, tokenA, tokenB, baseURL, tokenC string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET token_b = $2, base_url = $3, token_c = $4,module_endpoints='{}'::jsonb,
		        token_a = NULL, token_a_used_at = NOW(),
		        handshake_status = 'IN_PROGRESS'
		  WHERE id = $1 AND token_a=$5 AND status <> 'SUSPENDED'`, cpoID, tokenB, baseURL, tokenC, tokenA)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// UpdateCPOCredentials stores a rotation (`PUT /credentials`): the CPO replaced
// its Token B and ChargeNet answers with a new Token C. Token A is left alone —
// it was spent when the partner registered.
func (s *Store) UpdateCPOCredentials(ctx context.Context, cpoID, tokenB, baseURL, tokenC string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET token_b = $2, base_url = $3, token_c = $4,module_endpoints='{}'::jsonb, handshake_status = 'IN_PROGRESS'
		  WHERE id = $1`, cpoID, tokenB, baseURL, tokenC)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// SuspendCPO unregisters a partner (`DELETE /credentials`). Both tokens are
// dropped so it cannot authenticate again without a new Token A, and the status
// turns SUSPENDED — which is what hides its stations from the driver app, since
// station discovery filters on `cpo_partners.status = 'ACTIVE'`.
func (s *Store) SuspendCPO(ctx context.Context, cpoID string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET status = 'SUSPENDED', token_a = NULL, token_b = NULL, token_c = NULL,
		        handshake_status = 'NOT_STARTED'
		  WHERE id = $1`, cpoID)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// ListActiveCPOs returns every ACTIVE partner that has the OCPI credentials a
// pull needs — a base URL and a Token B. It is what the periodic
// locations/tariffs sync iterates; a partner still mid-registration, or one that
// has been suspended, is skipped because there is nothing (and nowhere) to pull.
func (s *Store) ListActiveCPOs(ctx context.Context) ([]*CpoAccount, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+cpoColumns+` FROM cpo_partners
		  WHERE status = 'ACTIVE' AND base_url <> ''
		    AND token_b IS NOT NULL AND token_b <> ''
		  ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	var cpos []*CpoAccount
	for rows.Next() {
		c, err := scanCPO(rows)
		if err != nil {
			return nil, err
		}
		cpos = append(cpos, c)
	}
	return cpos, rows.Err()
}

// scanCPO maps one cpo_partners row, folding the nullable token columns into
// empty strings so callers never have to handle sql.Null*. It takes the shared
// [rowScanner] so one mapping serves both a single QueryRow and a Query loop.
func scanCPO(row rowScanner) (*CpoAccount, error) {
	var endpointJSON []byte
	var (
		c                      CpoAccount
		tokenA, tokenB, tokenC sql.NullString
		ocpiVersion, handshake sql.NullString
		tokenAUsedAt           sql.NullTime
	)
	err := row.Scan(&c.ID, &c.Name, &c.Slug, &c.CountryCode, &c.PartyID, &c.BaseURL,
		&c.ColorHex, &c.ContactEmail, &tokenA, &tokenAUsedAt, &tokenB, &tokenC,
		&ocpiVersion, &handshake, &c.PlatformFeePct, &c.Status, &c.CreatedAt, &endpointJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.TokenA, c.TokenB, c.TokenC = tokenA.String, tokenB.String, tokenC.String
	c.OCPIVersion, c.HandshakeStatus = ocpiVersion.String, handshake.String
	if len(endpointJSON) > 0 {
		if err := json.Unmarshal(endpointJSON, &c.ModuleEndpoints); err != nil {
			return nil, err
		}
	}
	if tokenAUsedAt.Valid {
		used := tokenAUsedAt.Time
		c.TokenAUsedAt = &used
	}
	return &c, nil
}

// requireOneRow turns "the UPDATE matched nothing" into ErrNotFound, so a
// handler can answer 404 instead of silently reporting success.
func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

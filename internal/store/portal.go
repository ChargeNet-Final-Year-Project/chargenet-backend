// Package store contains the application's database access.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Portal role values.
const (
	PortalRoleSuperAdmin = "SUPER_ADMIN"
	PortalRoleCPO        = "CPO"
)

// Payout cycle values.
const (
	PayoutWeekly  = "WEEKLY"
	PayoutMonthly = "MONTHLY"
)

// PayoutCycles is the accepted set, for handler validation.
var PayoutCycles = []string{PayoutWeekly, PayoutMonthly}

// PortalUser is a CPO-portal login. PasswordHash never leaves the backend: the
// handler builds its own DTO, exactly as it does for a driver.
type PortalUser struct {
	ID           string
	Email        string
	PasswordHash string
	Name         string
	Role         string
	// CPOID links a CPO-role login to its partner. A super admin has none.
	CPOID     string
	CreatedAt time.Time
}

// portalUserColumns is the select list scanPortalUser expects.
const portalUserColumns = `id, email, password_hash, name, role, coalesce(cpo_id, ''), created_at`

// GetPortalUserByEmail looks a portal login up by (case-insensitive) email.
func (s *Store) GetPortalUserByEmail(ctx context.Context, email string) (*PortalUser, error) {
	return s.scanPortalUser(ctx, `SELECT `+portalUserColumns+` FROM portal_users WHERE email = $1`, normalizeEmail(email))
}

// GetPortalUserByID looks a portal login up by id (used to refresh `GET /me`).
func (s *Store) GetPortalUserByID(ctx context.Context, id string) (*PortalUser, error) {
	return s.scanPortalUser(ctx, `SELECT `+portalUserColumns+` FROM portal_users WHERE id = $1`, id)
}

func (s *Store) scanPortalUser(ctx context.Context, query string, arg any) (*PortalUser, error) {
	u := &PortalUser{}
	err := s.DB.QueryRowContext(ctx, query, arg).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.CPOID, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// CountPortalUsers reports how many portal logins exist, which is what decides
// whether the portal seed runs (mirrors [Store.CountUsers]).
func (s *Store) CountPortalUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM portal_users`).Scan(&n)
	return n, err
}

// CPOProfileView is a CPO's own company profile as the portal sees it. The live
// OCPI tokens are never returned. Only their presence is exposed, except Token A,
// which the CPO needs to hand to its operator (and which stops being readable
// once the credential exchange consumes it).
type CPOProfileView struct {
	OCPIVersion  string `json:"ocpiVersion"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	CountryCode  string `json:"countryCode"`
	PartyID      string `json:"partyId"`
	BaseURL      string `json:"baseUrl"`
	ColorHex     string `json:"colorHex"`
	ContactEmail string `json:"contactEmail"`
	TokenA       string `json:"tokenA"`
	TokenAUsed   bool   `json:"tokenAUsed"`
	TokenBSet    bool   `json:"tokenBSet"`
	TokenCSet    bool   `json:"tokenCSet"`
	// TokenBLast4/TokenCLast4 are the only parts of Tokens B and C exposed.
	TokenBLast4       string    `json:"tokenBLast4"`
	TokenCLast4       string    `json:"tokenCLast4"`
	HandshakeStatus   string    `json:"handshakeStatus"`
	Status            string    `json:"status"`
	PlatformFeePct    float64   `json:"platformFeePct"`
	BankName          string    `json:"bankName"`
	BankAccountHolder string    `json:"bankAccountHolder"`
	BankAccountNo     string    `json:"bankAccountNo"`
	PayoutCycle       string    `json:"payoutCycle"`
	CreatedAt         time.Time `json:"createdAt"`
	// HandshakeAt is nil until the first handshake test.
	HandshakeAt *time.Time `json:"handshakeAt"`
}

// GetCPOProfile returns ErrNotFound for a missing row so the handler can answer 404.
func (s *Store) GetCPOProfile(ctx context.Context, cpoID string) (*CPOProfileView, error) {
	p := &CPOProfileView{}
	var tokenA, tokenB, tokenC, bankName, holder, accountNo, cycle sql.NullString
	var handshakeAt sql.NullTime
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, slug, country_code, party_id, base_url, color_hex, contact_email,
		        token_a, token_b, token_c, handshake_status, status, platform_fee_pct,
		        bank_name, bank_account_holder, bank_account_no, payout_cycle, created_at,
		        (token_a_used_at IS NOT NULL), handshake_at,coalesce(ocpi_version,'2.3.0')
		   FROM cpo_partners WHERE id = $1`, cpoID).
		Scan(&p.ID, &p.Name, &p.Slug, &p.CountryCode, &p.PartyID, &p.BaseURL, &p.ColorHex,
			&p.ContactEmail, &tokenA, &tokenB, &tokenC, &p.HandshakeStatus, &p.Status,
			&p.PlatformFeePct, &bankName, &holder, &accountNo, &cycle, &p.CreatedAt, &p.TokenAUsed,
			&handshakeAt, &p.OCPIVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.TokenA = strings.TrimSpace(tokenA.String)
	p.TokenBSet = strings.TrimSpace(tokenB.String) != ""
	p.TokenCSet = strings.TrimSpace(tokenC.String) != ""
	p.TokenBLast4 = last4(tokenB.String)
	p.TokenCLast4 = last4(tokenC.String)
	p.BankName, p.BankAccountHolder = bankName.String, holder.String
	p.BankAccountNo, p.PayoutCycle = accountNo.String, cycle.String
	if handshakeAt.Valid {
		at := handshakeAt.Time
		p.HandshakeAt = &at
	}
	return p, nil
}

// last4 renders the last four characters of a stored secret for display, or ""
// when it is unset. It is the only part of Tokens B/C the console may see.
func last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}

// CPOProfileUpdate carries the writable fields of `PUT /admin/api/cpo`. An empty
// BankAccountNo means "unchanged" (the console sends the masked number straight
// back when the user did not touch the billing fields).
type CPOProfileUpdate struct {
	Name              string
	BaseURL           string
	ContactEmail      string
	ColorHex          string
	BankName          string
	BankAccountHolder string
	BankAccountNo     string
	PayoutCycle       string
}

// UpdateCPOProfile writes the editable profile fields. COALESCE/NULLIF keeps the
// stored account number when the caller did not resupply it.
func (s *Store) UpdateCPOProfile(ctx context.Context, cpoID string, in CPOProfileUpdate) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET name = $2, module_endpoints=CASE WHEN base_url IS DISTINCT FROM $3 THEN '{}'::jsonb ELSE module_endpoints END,base_url = $3, contact_email = $4, color_hex = $5,
		        bank_name = $6, bank_account_holder = $7, payout_cycle = $8,
		        bank_account_no = COALESCE(NULLIF($9, ''), bank_account_no)
		  WHERE id = $1`,
		cpoID, in.Name, in.BaseURL, in.ContactEmail, in.ColorHex,
		in.BankName, in.BankAccountHolder, in.PayoutCycle, in.BankAccountNo)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// ErrHandshakeVerified is returned when a Token A rotation is asked for a partner
// whose handshake already succeeded — a verified connection cannot be re-registered.
var ErrHandshakeVerified = errors.New("store: handshake already verified")

// RegenerateTokenA issues a fresh Token A and clears the handshake. It refuses once the partner is VERIFIED, in SQL as well as in the
// handler, so a stale console tab cannot undo a live integration.
func (s *Store) RegenerateTokenA(ctx context.Context, cpoID, tokenA string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET token_a = $2, token_a_used_at = NULL, handshake_status = 'NOT_STARTED'
		  WHERE id = $1 AND handshake_status <> 'VERIFIED'`, cpoID, tokenA)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, gerr := s.GetCPOProfile(ctx, cpoID); errors.Is(gerr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrHandshakeVerified
	}
	return nil
}

// SetHandshakeResult stores what a portal handshake test found:
// the technical status and ordered step log. Marketplace approval belongs to
// the administrator and cannot be changed by operator-run verification.
func (s *Store) SetHandshakeResult(ctx context.Context, cpoID, status string, log []byte) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET handshake_status = $2,
		        handshake_log = $3,
		        handshake_at = NOW()
		  WHERE id = $1`, cpoID, status, log)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// DisconnectCPO clears a partner's stored OCPI credentials (Tokens B and C) and
// returns it to the pre-handshake state — the portal's stand-in for the OCPI
// credentials DELETE. The partner keeps its identity, network
// and billing; a fresh handshake is required to reconnect.
func (s *Store) DisconnectCPO(ctx context.Context, cpoID string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET token_b = NULL,
		        token_c = NULL,
		        handshake_status = 'NOT_STARTED',
		        handshake_log = NULL
		  WHERE id = $1`, cpoID)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// PortalRegisterInput is a validated `POST /admin/api/register` body, already
// checked and hashed by the handler. TokenA is generated by the caller so the
// store never has to know the token format.
type PortalRegisterInput struct {
	CompanyName       string
	Slug              string
	CountryCode       string
	PartyID           string
	BaseURL           string
	ColorHex          string
	ContactEmail      string
	ContactName       string
	Email             string
	PasswordHash      string
	BankName          string
	BankAccountHolder string
	BankAccountNo     string
	PayoutCycle       string
	TokenA            string
	PlatformFeePct    float64
}

// ErrSlugTaken / ErrPartyIDTaken are the two uniqueness collisions a
// self-registration can hit besides the email.
var (
	ErrSlugTaken    = errors.New("store: slug already taken")
	ErrPartyIDTaken = errors.New("store: party id already taken")
)

// RegisterCPO creates a PENDING partner and its CPO-role portal login in one
// transaction, so a half-registered company can never exist. The partner starts
// PENDING/NOT_STARTED; the handshake test is what activates it.
func (s *Store) RegisterCPO(ctx context.Context, in PortalRegisterInput) (*CPOProfileView, *PortalUser, error) {
	cpoID := NewID("cpo")
	userID := NewID("pu")
	email := normalizeEmail(in.Email)
	color := strings.TrimSpace(in.ColorHex)
	if color == "" {
		color = "#6B7280"
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cpo_partners
		   (id, name, slug, country_code, party_id, base_url, color_hex, contact_email,
		    token_a, handshake_status, status, platform_fee_pct,
		    bank_name, bank_account_holder, bank_account_no, payout_cycle)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'NOT_STARTED', 'PENDING', $10, $11, $12, $13, $14)`,
		cpoID, strings.TrimSpace(in.CompanyName), strings.TrimSpace(in.Slug),
		strings.ToUpper(strings.TrimSpace(in.CountryCode)), strings.ToUpper(strings.TrimSpace(in.PartyID)),
		strings.TrimSpace(in.BaseURL), color, strings.TrimSpace(in.ContactEmail), in.TokenA,
		in.PlatformFeePct, strings.TrimSpace(in.BankName), strings.TrimSpace(in.BankAccountHolder),
		strings.TrimSpace(in.BankAccountNo), strings.TrimSpace(in.PayoutCycle)); err != nil {
		switch {
		case isUniqueViolation(err, "cpo_partners_slug_key"):
			return nil, nil, ErrSlugTaken
		case isUniqueViolation(err, "cpo_partners_country_code_party_id_key"):
			return nil, nil, ErrPartyIDTaken
		}
		return nil, nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO portal_users (id, email, password_hash, name, role, cpo_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, email, in.PasswordHash, strings.TrimSpace(in.ContactName), PortalRoleCPO, cpoID); err != nil {
		if isUniqueViolation(err, "portal_users_email_key") {
			return nil, nil, ErrEmailTaken
		}
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	profile, err := s.GetCPOProfile(ctx, cpoID)
	if err != nil {
		return nil, nil, err
	}
	user, err := s.GetPortalUserByID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	return profile, user, nil
}

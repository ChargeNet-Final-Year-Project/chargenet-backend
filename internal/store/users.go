package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User is a driver account. PasswordHash is internal and never serialised to
// JSON (handlers build the {id,name,email,createdAt} DTO explicitly).
type User struct {
	ID           string
	Name         string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// CreateUser inserts a driver and their (empty) wallet in one transaction.
// Emails are normalised to lower-case; a duplicate returns ErrEmailTaken.
func (s *Store) CreateUser(ctx context.Context, name, email, passwordHash string, currencies ...string) (*User, error) {
	email = normalizeEmail(email)
	user := &User{
		ID:           NewID("usr"),
		Name:         strings.TrimSpace(name),
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}
	walletID := NewID("wal")

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (id, name, email, password_hash) VALUES ($1, $2, $3, $4)`,
		user.ID, user.Name, user.Email, user.PasswordHash,
	); err != nil {
		if isUniqueViolation(err, "users_email_key") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wallets (id, user_id, balance, currency) VALUES ($1, $2, 0.00, $3)`,
		walletID, user.ID, reportCurrency(currencies),
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return user, nil
}

// GetUserByEmail looks a driver up by (case-insensitive) email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	return s.scanUser(ctx, `SELECT id, name, email, password_hash, created_at FROM users WHERE email = $1`, normalizeEmail(email))
}

// GetUserByID looks a driver up by id.
func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	return s.scanUser(ctx, `SELECT id, name, email, password_hash, created_at FROM users WHERE id = $1`, id)
}

func (s *Store) scanUser(ctx context.Context, query string, arg any) (*User, error) {
	u := &User{}
	err := s.DB.QueryRowContext(ctx, query, arg).
		Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// CountUsers returns the number of driver rows (used to decide on seeding).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

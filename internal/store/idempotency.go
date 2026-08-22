package store

import (
	"context"
	"database/sql"
	"errors"
)

// IdempotencyRecord is a previously stored response for an idempotent request.
// Status 0 means a reservation is still in flight (the handler has not
// finished yet).
type IdempotencyRecord struct {
	Status int
	Body   []byte
}

// IdempotencyInFlightStatus marks a reserved-but-unfinished idempotency row.
const IdempotencyInFlightStatus = 0

// inFlightStatus is the package-local alias used by the SQL above.
const inFlightStatus = IdempotencyInFlightStatus

// ReserveIdempotency claims (key, userID, endpoint) for this caller. It returns
// true when the caller created the reservation and false when another request
// already owns (or owned) it. The placeholder is replaced by SaveIdempotency
// once the handler has produced a response.
func (s *Store) ReserveIdempotency(ctx context.Context, key, userID, endpoint string) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, user_id, endpoint, response_status, response_body)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb)
		 ON CONFLICT (key, user_id, endpoint) DO NOTHING`,
		key, userID, endpoint, inFlightStatus)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// GetIdempotency returns the stored response for a key, ErrNotFound when the
// key was never seen, or a record with Status 0 while a request is in flight.
func (s *Store) GetIdempotency(ctx context.Context, key, userID, endpoint string) (*IdempotencyRecord, error) {
	rec := &IdempotencyRecord{}
	err := s.DB.QueryRowContext(ctx,
		`SELECT response_status, response_body FROM idempotency_keys
		  WHERE key = $1 AND user_id = $2 AND endpoint = $3`,
		key, userID, endpoint).Scan(&rec.Status, &rec.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// SaveIdempotency stores the final status and body for a reservation.
func (s *Store) SaveIdempotency(ctx context.Context, key, userID, endpoint string, status int, body []byte) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE idempotency_keys
		    SET response_status = $4, response_body = $5::jsonb
		  WHERE key = $1 AND user_id = $2 AND endpoint = $3`,
		key, userID, endpoint, status, string(body))
	return err
}

// ReleaseIdempotency drops a reservation so a later retry can execute again.
// It is used when a handler failed and we do not want to cache the failure.
func (s *Store) ReleaseIdempotency(ctx context.Context, key, userID, endpoint string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM idempotency_keys
		  WHERE key = $1 AND user_id = $2 AND endpoint = $3 AND response_status = $4`,
		key, userID, endpoint, inFlightStatus)
	return err
}

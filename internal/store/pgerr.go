package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolation is PostgreSQL's SQLSTATE for unique_violation.
const pgUniqueViolation = "23505"

// isUniqueViolation reports whether err is a unique-constraint violation, and
// which constraint name triggered it (if available).
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != pgUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

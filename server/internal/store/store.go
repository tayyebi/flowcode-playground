// Package store is typed CRUD over the platform's PostgreSQL schema, one
// file per entity (users/workspaces, content, settings, administrators). It
// holds no HTTP or execution logic — that lives in the binaries' handler
// files and package engine, respectively.
package store

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Store is typed CRUD over the platform's PostgreSQL database.
type Store struct {
	db *sql.DB
}

// New wraps an already-open, already-migrated database.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// failure (SQLSTATE 23505), e.g. a slug collision the caller wants to retry
// with a different suffix.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ErrNotFound is returned by Get-style lookups when no row matches.
var ErrNotFound = sql.ErrNoRows

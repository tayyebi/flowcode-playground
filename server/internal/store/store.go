// Package store is typed CRUD over the platform's SQLite schema, one file
// per entity (projects, files, versions, deployments, triggers, executions,
// kv). It holds no HTTP or execution logic — that lives in package main's
// handler files and package engine, respectively.
package store

import (
	"database/sql"
	"strings"
)

// Store is typed CRUD over the platform's SQLite database.
type Store struct {
	db *sql.DB
}

// New wraps an already-open, already-migrated database.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint
// failure. modernc.org/sqlite doesn't export a stable error-code type across
// versions worth depending on here, so this matches on the driver's message
// text, which has been stable across SQLite's own releases for over a decade.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ErrNotFound is returned by Get-style lookups when no row matches.
var ErrNotFound = sql.ErrNoRows

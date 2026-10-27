// Package db owns the platform's PostgreSQL schema and startup migrations.
package db

import (
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open connects to the PostgreSQL database at dsn, applies any pending
// migrations (including each migration's inline seed rows), and returns the
// pool. maxOpen caps this binary's connections: the app and admin binaries
// each keep their own small pool against the shared server.
func Open(dsn string, maxOpen int) (*sql.DB, error) {
	if maxOpen <= 0 {
		maxOpen = 8
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxOpen)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}
	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return sqlDB, nil
}

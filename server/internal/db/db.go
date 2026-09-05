// Package db owns the platform's SQLite schema and startup migrations.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens (creating if necessary) the SQLite database at path, applies
// pragmas suited to a single-writer embedded workload, and applies any
// pending migrations.
func Open(path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite serializes writers regardless of connection pool size; at this
	// project's scale it is simpler to serialize every access than to tune
	// around lock contention, and it avoids "database is locked" errors
	// entirely rather than retrying around them.
	sqlDB.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := sqlDB.Exec(pragma); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return sqlDB, nil
}

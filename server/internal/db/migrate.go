package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies every embedded migration that schema_migrations doesn't
// already record, in numeric order, each inside its own transaction.
func migrate(sqlDB *sql.DB) error {
	if _, err := sqlDB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	type migration struct {
		version int
		name    string
	}
	migrations := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		versionStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return fmt.Errorf("migration file %q does not start with NNNN_", e.Name())
		}
		v, err := strconv.Atoi(versionStr)
		if err != nil {
			return fmt.Errorf("migration file %q has a non-numeric version prefix: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: v, name: e.Name()})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })

	for _, m := range migrations {
		var applied int
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if applied > 0 {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", m.name, err)
		}

		tx, err := sqlDB.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(string(data)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, m.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

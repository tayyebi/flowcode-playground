// Package testdb gives tests a PostgreSQL-backed *sql.DB in a throwaway
// schema, so every test gets freshly migrated tables (including the
// migration seeders) with complete isolation.
//
// Set TEST_DATABASE_URL to a PostgreSQL the test run may create and drop
// schemas in, e.g.
//
//	docker run -d --rm --name pg-test -p 55432:5432 \
//	  -e POSTGRES_HOST_AUTH_METHOD=trust postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres@localhost:55432/postgres?sslmode=disable' go test ./...
//
// Without it, tests that need the database skip.
package testdb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/tayyebi/flowcode-playground/server/internal/db"
)

// Open returns an open, migrated connection pool bound to a fresh schema.
// The schema (and everything created in it) is dropped when the test ends.
func Open(t *testing.T) *sql.DB {
	t.Helper()

	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("TEST_DATABASE_URL not set — database tests need a PostgreSQL instance")
	}

	buf := make([]byte, 6)
	rand.Read(buf) //nolint: crypto/rand never fails on supported platforms
	schema := "test_" + hex.EncodeToString(buf)

	create, err := db.Open(baseDSN, 1)
	if err != nil {
		t.Fatalf("connect for schema setup: %v", err)
	}
	if _, err := create.Exec("CREATE SCHEMA " + schema); err != nil {
		create.Close()
		t.Fatalf("create schema: %v", err)
	}
	create.Close()
	t.Cleanup(func() {
		drop, err := db.Open(baseDSN, 1)
		if err != nil {
			t.Fatalf("connect for schema teardown: %v", err)
		}
		if _, err := drop.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Fatalf("drop schema: %v", err)
		}
		drop.Close()
	})

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	sqlDB, err := db.Open(u.String(), 4)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB
}

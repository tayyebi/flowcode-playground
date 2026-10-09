package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SeededExample is the bookkeeping row behind seed-once: it records that the
// example named Name was already turned into a project, so restarts skip it
// and an admin deleting the project through the UI is respected (the row
// stays, the project stays gone).
type SeededExample struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	ProjectID int64  `json:"projectId"`
	Hash      string `json:"hash"`
	SeededAt  string `json:"seededAt"`
}

func (s *Store) GetSeededExample(ctx context.Context, name string) (*SeededExample, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, slug, project_id, hash, seeded_at FROM seeded_examples WHERE name = ?`, name)
	var se SeededExample
	if err := row.Scan(&se.Name, &se.Slug, &se.ProjectID, &se.Hash, &se.SeededAt); err != nil {
		return nil, err
	}
	return &se, nil
}

// RecordSeededExample upserts the bookkeeping row once an example's project
// has been fully created (or adopted).
func (s *Store) RecordSeededExample(ctx context.Context, name, slug string, projectID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO seeded_examples (name, slug, project_id, hash) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			slug = excluded.slug, project_id = excluded.project_id,
			hash = excluded.hash, seeded_at = datetime('now')`,
		name, slug, projectID, hash)
	if err != nil {
		return fmt.Errorf("record seeded example: %w", err)
	}
	return nil
}

func (s *Store) DeleteSeededExample(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM seeded_examples WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete seeded example: %w", err)
	}
	return nil
}

// GetProjectBySlug finds a project by its exact slug. Used by the seeder's
// adoption path: a database seeded before seeding was built in has the
// project already, and it must be recognized rather than duplicated.
func (s *Store) GetProjectBySlug(ctx context.Context, slug string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

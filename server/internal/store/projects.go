package store

import (
	"context"
	"fmt"
	"strings"
)

// Project is a named, saved workspace: a folder of independently runnable
// FlowCode files plus their saved versions, deployments, and triggers.
type Project struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

const projectColumns = "id, slug, name, description, created_at, updated_at"

func scanProject(row rowScanner) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// CreateProject inserts a new project, deriving a URL-safe unique slug from
// name and appending a numeric suffix on collision.
func (s *Store) CreateProject(ctx context.Context, name, description string) (*Project, error) {
	base := slugify(name)
	if base == "" {
		base = "project"
	}

	for attempt := 0; attempt < 100; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO projects (slug, name, description) VALUES (?, ?, ?)`,
			slug, name, description)
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return nil, fmt.Errorf("insert project: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		return s.GetProject(ctx, id)
	}
	return nil, fmt.Errorf("could not find a unique slug for %q", name)
}

func (s *Store) GetProject(ctx context.Context, id int64) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	return scanProject(row)
}

func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	projects := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// UpdateProject applies non-nil fields and bumps updated_at.
func (s *Store) UpdateProject(ctx context.Context, id int64, name, description *string) (*Project, error) {
	if name != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE projects SET name = ?, updated_at = datetime('now') WHERE id = ?`, *name, id); err != nil {
			return nil, fmt.Errorf("update project name: %w", err)
		}
	}
	if description != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE projects SET description = ?, updated_at = datetime('now') WHERE id = ?`, *description, id); err != nil {
			return nil, fmt.Errorf("update project description: %w", err)
		}
	}
	return s.GetProject(ctx, id)
}

// DeleteProject removes a project and, via ON DELETE CASCADE, every file,
// version, deployment, trigger, execution, and kv row that belongs to it.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return nil
}

// touchProject bumps a project's updated_at, called whenever something
// inside it (a file, a version) changes, so the dashboard's "recently
// updated" ordering reflects real activity.
func (s *Store) touchProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET updated_at = datetime('now') WHERE id = ?`, id)
	return err
}

func slugify(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

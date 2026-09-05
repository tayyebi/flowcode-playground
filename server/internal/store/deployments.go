package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"fmt"
	"strings"
)

// Deployment publishes a project file as a public, slug-addressed HTTP
// endpoint (see the plan doc for the Phase A limitation: the incoming
// request's data is not available to the running workflow yet).
type Deployment struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	FileID    int64  `json:"fileId"`
	FileName  string `json:"fileName"`
	Slug      string `json:"slug"`
	VersionID *int64 `json:"versionId,omitempty"` // nil = always run the current draft
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

const deploymentColumns = `d.id, d.project_id, d.file_id, f.name, d.slug, d.version_id, d.enabled, d.created_at, d.updated_at`

func scanDeployment(row rowScanner) (*Deployment, error) {
	var d Deployment
	var enabled int
	var versionID sql.NullInt64
	if err := row.Scan(&d.ID, &d.ProjectID, &d.FileID, &d.FileName, &d.Slug, &versionID, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.Enabled = enabled != 0
	if versionID.Valid {
		d.VersionID = &versionID.Int64
	}
	return &d, nil
}

func deploymentQuery(where string) string {
	return `SELECT ` + deploymentColumns + ` FROM deployments d JOIN files f ON f.id = d.file_id WHERE ` + where
}

// randomSlug generates a short, URL-safe, hard-to-guess public identifier.
func randomSlug() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate slug: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

func (s *Store) CreateDeployment(ctx context.Context, projectID, fileID int64, versionID *int64) (*Deployment, error) {
	for attempt := 0; attempt < 10; attempt++ {
		slug, err := randomSlug()
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO deployments (project_id, file_id, slug, version_id) VALUES (?, ?, ?, ?)`,
			projectID, fileID, slug, versionID)
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return nil, fmt.Errorf("insert deployment: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		return s.GetDeployment(ctx, id)
	}
	return nil, fmt.Errorf("could not generate a unique deployment slug")
}

func (s *Store) GetDeployment(ctx context.Context, id int64) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, deploymentQuery("d.id = ?"), id)
	return scanDeployment(row)
}

func (s *Store) GetDeploymentBySlug(ctx context.Context, slug string) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, deploymentQuery("d.slug = ?"), slug)
	return scanDeployment(row)
}

func (s *Store) ListDeployments(ctx context.Context, projectID int64) ([]*Deployment, error) {
	rows, err := s.db.QueryContext(ctx, deploymentQuery("d.project_id = ?")+" ORDER BY d.created_at DESC", projectID)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	deployments := []*Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}

func (s *Store) SetDeploymentEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET enabled = ?, updated_at = datetime('now') WHERE id = ?`, v, id)
	return err
}

func (s *Store) DeleteDeployment(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM deployments WHERE id = ?`, id)
	return err
}

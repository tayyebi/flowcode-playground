package store

import (
	"context"
	"fmt"
)

// File is one independently compiled/run FlowCode source within a project.
type File struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Position  int    `json:"position"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

const fileColumns = "id, project_id, name, content, position, created_at, updated_at"

func scanFile(row rowScanner) (*File, error) {
	var f File
	if err := row.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Content, &f.Position, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Store) ListFiles(ctx context.Context, projectID int64) ([]*File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE project_id = ? ORDER BY position, name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	files := []*File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) GetFile(ctx context.Context, projectID int64, name string) (*File, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE project_id = ? AND name = ?`, projectID, name)
	return scanFile(row)
}

func (s *Store) GetFileByID(ctx context.Context, id int64) (*File, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE id = ?`, id)
	return scanFile(row)
}

// UpsertFile creates the file if it doesn't exist, or saves new content for
// one that does. New files are appended after the current highest position.
func (s *Store) UpsertFile(ctx context.Context, projectID int64, name, content string) (*File, error) {
	existing, err := s.GetFile(ctx, projectID, name)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE files SET content = ?, updated_at = datetime('now') WHERE id = ?`, content, existing.ID); err != nil {
			return nil, fmt.Errorf("update file: %w", err)
		}
		s.touchProject(ctx, projectID)
		return s.GetFileByID(ctx, existing.ID)
	}
	if err != ErrNotFound {
		return nil, fmt.Errorf("get file: %w", err)
	}

	var nextPos int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position) + 1, 0) FROM files WHERE project_id = ?`, projectID).Scan(&nextPos); err != nil {
		return nil, fmt.Errorf("compute next position: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO files (project_id, name, content, position) VALUES (?, ?, ?, ?)`,
		projectID, name, content, nextPos)
	if err != nil {
		return nil, fmt.Errorf("insert file: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	s.touchProject(ctx, projectID)
	return s.GetFileByID(ctx, id)
}

func (s *Store) DeleteFile(ctx context.Context, projectID int64, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE project_id = ? AND name = ?`, projectID, name); err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	return s.touchProject(ctx, projectID)
}

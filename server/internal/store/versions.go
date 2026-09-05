package store

import (
	"context"
	"fmt"
)

// Version is an immutable snapshot of every file in a project, taken by a
// "Save Version" action.
type Version struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	Number    int    `json:"number"`
	Label     string `json:"label"`
	CreatedAt string `json:"createdAt"`
}

// VersionFile is one file's content as frozen inside a Version snapshot.
type VersionFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func scanVersion(row rowScanner) (*Version, error) {
	var v Version
	if err := row.Scan(&v.ID, &v.ProjectID, &v.Number, &v.Label, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

const versionColumns = "id, project_id, number, label, created_at"

// CreateVersion snapshots every current file of a project into a new,
// immutable version. Content is denormalized (name+content, not a files.id
// FK) so the snapshot stays readable even after a file is renamed or deleted.
func (s *Store) CreateVersion(ctx context.Context, projectID int64, label string) (*Version, error) {
	files, err := s.ListFiles(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list files to snapshot: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("project has no files to snapshot")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var nextNumber int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(number) + 1, 1) FROM versions WHERE project_id = ?`, projectID).Scan(&nextNumber); err != nil {
		return nil, fmt.Errorf("compute next version number: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO versions (project_id, number, label) VALUES (?, ?, ?)`, projectID, nextNumber, label)
	if err != nil {
		return nil, fmt.Errorf("insert version: %w", err)
	}
	versionID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}

	for _, f := range files {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO version_files (version_id, name, content) VALUES (?, ?, ?)`,
			versionID, f.Name, f.Content); err != nil {
			return nil, fmt.Errorf("insert version file %s: %w", f.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetVersion(ctx, versionID)
}

func (s *Store) GetVersion(ctx context.Context, id int64) (*Version, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM versions WHERE id = ?`, id)
	return scanVersion(row)
}

func (s *Store) GetVersionByNumber(ctx context.Context, projectID int64, number int) (*Version, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM versions WHERE project_id = ? AND number = ?`, projectID, number)
	return scanVersion(row)
}

func (s *Store) ListVersions(ctx context.Context, projectID int64) ([]*Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+versionColumns+` FROM versions WHERE project_id = ? ORDER BY number DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	versions := []*Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *Store) ListVersionFiles(ctx context.Context, versionID int64) ([]*VersionFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, content FROM version_files WHERE version_id = ? ORDER BY name`, versionID)
	if err != nil {
		return nil, fmt.Errorf("list version files: %w", err)
	}
	defer rows.Close()

	files := []*VersionFile{}
	for rows.Next() {
		var f VersionFile
		if err := rows.Scan(&f.Name, &f.Content); err != nil {
			return nil, err
		}
		files = append(files, &f)
	}
	return files, rows.Err()
}

// GetVersionFileContent looks up one file's frozen content within a version,
// used to resolve what a pinned deployment or trigger should actually run.
func (s *Store) GetVersionFileContent(ctx context.Context, versionID int64, name string) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM version_files WHERE version_id = ? AND name = ?`, versionID, name).Scan(&content)
	if err != nil {
		return "", err
	}
	return content, nil
}

// RestoreVersion overwrites a project's current working files with a
// version's frozen snapshot. Files present only in the working copy (added
// after the snapshot was taken) are left alone; a file the snapshot has that
// the working copy has since dropped is recreated.
func (s *Store) RestoreVersion(ctx context.Context, projectID, versionID int64) error {
	files, err := s.ListVersionFiles(ctx, versionID)
	if err != nil {
		return fmt.Errorf("list version files: %w", err)
	}
	for _, f := range files {
		if _, err := s.UpsertFile(ctx, projectID, f.Name, f.Content); err != nil {
			return fmt.Errorf("restore file %s: %w", f.Name, err)
		}
	}
	return nil
}

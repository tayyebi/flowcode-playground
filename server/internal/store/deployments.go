package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"fmt"
	"strings"
)

// Deployment publishes a project file as a public, slug-addressed HTTP
// endpoint; rows live in ws_deployments.
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

func scanDeployment(row rowScanner) (*Deployment, error) {
	var d Deployment
	var versionID sql.NullInt64
	if err := row.Scan(&d.ID, &d.ProjectID, &d.FileID, &d.FileName, &d.Slug, &versionID, &d.Enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if versionID.Valid {
		d.VersionID = &versionID.Int64
	}
	return &d, nil
}

// randomSlug generates a short, URL-safe, hard-to-guess public identifier.
func randomSlug() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate slug: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

package store

import (
	"strings"
)

// Project is the project payload embedded in ProjectAccess (see
// workspaces.go); rows live in ws_projects.
type Project struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspaceId,omitempty"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// Slugify derives a URL-safe slug from a project name, appending numeric
// suffixes on collision (handled by the callers' retry loops).
func Slugify(name string) string {
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

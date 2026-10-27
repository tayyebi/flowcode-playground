package store

// Version is an immutable snapshot of every file in a project, taken by a
// "Save Version" action; rows live in ws_versions / ws_version_files.
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

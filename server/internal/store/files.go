package store

// File is one independently compiled/run FlowCode source within a project;
// rows live in ws_files.
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

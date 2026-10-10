package store

import (
	"context"
	"fmt"
)

// Role is a permission level, used for both workspace membership
// (owner/admin/member) and per-project sharing (owner/editor/viewer). The
// two vocabularies are kept in separate constants because they are checked
// independently: EffectiveProjectRole resolves one from the other.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// User is an OIDC-authenticated identity, upserted on every login keyed by
// the provider's `sub` claim. IsAdmin is promoted manually from the database
// (UPDATE users SET is_admin = 1 WHERE email = '…'); there is no promote UI.
type User struct {
	ID          int64  `json:"id"`
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Picture     string `json:"picture"`
	IsAdmin     bool   `json:"isAdmin"`
	CreatedAt   string `json:"createdAt"`
	LastLoginAt string `json:"lastLoginAt,omitempty"`
}

// Workspace is the top-level account boundary — the thing addressed in URLs
// as /#/w/<slug>/... (Cloudflare-style). Every project belongs to exactly
// one workspace; sharing happens between its members.
type Workspace struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

// WorkspaceMember pairs a user with their role inside a workspace.
type WorkspaceMember struct {
	WorkspaceID int64  `json:"workspaceId"`
	UserID      int64  `json:"userId"`
	Role        Role   `json:"role"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	Picture     string `json:"picture"`
}

// ProjectShare grants one member a role on one project. No share row means
// the member's effective role falls back to their workspace role.
type ProjectShare struct {
	ProjectID int64  `json:"projectId"`
	UserID    int64  `json:"userId"`
	Role      Role   `json:"role"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

const userColumns = "id, subject, email, name, picture, is_admin, created_at, COALESCE(last_login_at, '')"

func scanUser(row rowScanner) (*User, error) {
	var u User
	var isAdmin int
	if err := row.Scan(&u.ID, &u.Subject, &u.Email, &u.Name, &u.Picture, &isAdmin, &u.CreatedAt, &u.LastLoginAt); err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin != 0
	return &u, nil
}

// ListUsers returns every user, for the admin Users page.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpsertUserBySubject creates the user for an OIDC `sub` on first login and
// refreshes profile claims + last_login_at on every login.
func (s *Store) UpsertUserBySubject(ctx context.Context, subject, email, name, picture string) (*User, error) {
	if subject == "" {
		return nil, fmt.Errorf("user subject must not be empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (subject, email, name, picture, last_login_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(subject) DO UPDATE SET
			email = CASE WHEN excluded.email != '' THEN excluded.email ELSE users.email END,
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE users.name END,
			picture = CASE WHEN excluded.picture != '' THEN excluded.picture ELSE users.picture END,
			last_login_at = datetime('now')`,
		subject, email, name, picture)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	return s.GetUserBySubject(ctx, subject)
}

func (s *Store) GetUserBySubject(ctx context.Context, subject string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE subject = ?`, subject)
	return scanUser(row)
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

const workspaceColumns = "id, slug, name, created_at"

func scanWorkspace(row rowScanner) (*Workspace, error) {
	var w Workspace
	if err := row.Scan(&w.ID, &w.Slug, &w.Name, &w.CreatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWorkspace inserts a workspace with a unique URL slug derived from
// its name, appending a numeric suffix on collision (same convention as
// project slugs).
func (s *Store) CreateWorkspace(ctx context.Context, name string) (*Workspace, error) {
	base := Slugify(name)
	if base == "" {
		base = "workspace"
	}
	for attempt := 0; attempt < 100; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO workspaces (slug, name) VALUES (?, ?)`, slug, name)
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return nil, fmt.Errorf("insert workspace: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		return s.GetWorkspace(ctx, id)
	}
	return nil, fmt.Errorf("could not find a unique slug for %q", name)
}

func (s *Store) GetWorkspace(ctx context.Context, id int64) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

func (s *Store) GetWorkspaceBySlug(ctx context.Context, slug string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE slug = ?`, slug)
	return scanWorkspace(row)
}

func (s *Store) ListWorkspacesForUser(ctx context.Context, userID int64) ([]*Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+stringComma(workspaceColumns, "w.")+`
		FROM workspaces w JOIN workspace_members m ON m.workspace_id = w.id
		WHERE m.user_id = ? ORDER BY w.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	out := []*Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWorkspace(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

// AddWorkspaceMember inserts or updates a member's role. SQLite has no
// CHECK-aware upsert shortcut that reads naturally here, so delete-then-
// insert inside one statement pair via INSERT OR REPLACE.
func (s *Store) AddWorkspaceMember(ctx context.Context, workspaceID, userID int64, role Role) error {
	if !validWorkspaceRole(role) {
		return fmt.Errorf("invalid workspace role %q", role)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, ?)
		ON CONFLICT(workspace_id, user_id) DO UPDATE SET role = excluded.role`,
		workspaceID, userID, role)
	if err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	return nil
}

func (s *Store) RemoveWorkspaceMember(ctx context.Context, workspaceID, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID)
	return err
}

// LastOwnerError sentinel-ish check helpers ---------------------------------

func (s *Store) GetWorkspaceRole(ctx context.Context, workspaceID, userID int64) (Role, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`,
		workspaceID, userID).Scan(&role)
	if err != nil {
		return "", err
	}
	return Role(role), nil
}

func (s *Store) CountWorkspaceOwners(ctx context.Context, workspaceID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND role = 'owner'`,
		workspaceID).Scan(&n)
	return n, err
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, workspaceID int64) ([]*WorkspaceMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.workspace_id, m.user_id, m.role, u.email, u.name, u.picture
		FROM workspace_members m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ?
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, u.name`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	out := []*WorkspaceMember{}
	for rows.Next() {
		var m WorkspaceMember
		var role string
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &role, &m.Email, &m.Name, &m.Picture); err != nil {
			return nil, err
		}
		m.Role = Role(role)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// FindUserByEmailExact looks up a user by their stored email, case-
// insensitively, for invite-by-email. Returns ErrNotFound when absent.
func (s *Store) FindUserByEmailExact(ctx context.Context, email string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(email) = lower(?) AND email != ''`, email)
	return scanUser(row)
}

// Project-level sharing -------------------------------------------------------

// UpsertProjectShare grants (or changes) a user's role on a project. A share
// with role viewer/editor/owner only makes sense inside the project's
// workspace; callers must verify membership first (handler does).
func (s *Store) UpsertProjectShare(ctx context.Context, projectID, userID int64, role Role, grantedBy int64) error {
	if role != RoleOwner && role != RoleEditor && role != RoleViewer {
		return fmt.Errorf("invalid project share role %q", role)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_shares (project_id, user_id, role, granted_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, user_id) DO UPDATE SET role = excluded.role, granted_by = excluded.granted_by`,
		projectID, userID, role, grantedBy)
	if err != nil {
		return fmt.Errorf("upsert project share: %w", err)
	}
	return nil
}

func (s *Store) DeleteProjectShare(ctx context.Context, projectID, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM project_shares WHERE project_id = ? AND user_id = ?`, projectID, userID)
	return err
}

func (s *Store) ListProjectShares(ctx context.Context, projectID int64) ([]*ProjectShare, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.project_id, s.user_id, s.role, u.email, u.name
		FROM project_shares s JOIN users u ON u.id = s.user_id
		WHERE s.project_id = ? ORDER BY u.name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list shares: %w", err)
	}
	defer rows.Close()

	out := []*ProjectShare{}
	for rows.Next() {
		var sh ProjectShare
		var role string
		if err := rows.Scan(&sh.ProjectID, &sh.UserID, &role, &sh.Email, &sh.Name); err != nil {
			return nil, err
		}
		sh.Role = Role(role)
		out = append(out, &sh)
	}
	return out, rows.Err()
}

// EffectiveProjectRole answers "what may this user do with this project?" —
// the single authorization decision every ws_* handler goes through.
//
// Precedence: an explicit project_shares row wins; otherwise a workspace
// owner/admin is effectively an owner of every project in the workspace and
// a plain member is effectively an editor (the Cloudflare model: workspace
// admins can touch everything, members collaborate by default). A user who
// is not a workspace member gets "" (no access) regardless of shares —
// sharing stays inside the workspace boundary.
func (s *Store) EffectiveProjectRole(ctx context.Context, projectID, userID int64) (Role, error) {
	var wsID int64
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM ws_projects WHERE id = ?`, projectID).Scan(&wsID)
	if err != nil {
		return "", err // ErrNotFound if the project doesn't exist
	}
	var wsRole string
	err = s.db.QueryRowContext(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, wsID, userID).Scan(&wsRole)
	if err == ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace role: %w", err)
	}

	var shareRole string
	err = s.db.QueryRowContext(ctx,
		`SELECT role FROM project_shares WHERE project_id = ? AND user_id = ?`, projectID, userID).Scan(&shareRole)
	if err == nil {
		return Role(shareRole), nil
	}
	if err != ErrNotFound {
		return "", fmt.Errorf("project share: %w", err)
	}

	switch Role(wsRole) {
	case RoleOwner, RoleAdmin:
		return RoleOwner, nil
	default:
		return RoleEditor, nil
	}
}

// ListProjectsForUser returns every project the user can see across all
// their workspaces (own + shared), each annotated with the caller's
// effective role. Used by the dashboard.
func (s *Store) ListProjectsForUser(ctx context.Context, userID int64) ([]*ProjectAccess, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.workspace_id, p.slug, p.name, p.description, p.created_at, p.updated_at,
		       w.slug, w.name,
		       COALESCE(s.role, CASE m.role WHEN 'owner' THEN 'owner' WHEN 'admin' THEN 'owner' ELSE 'editor' END)
		FROM ws_projects p
		JOIN workspaces w ON w.id = p.workspace_id
		JOIN workspace_members m ON m.workspace_id = p.workspace_id AND m.user_id = ?
		LEFT JOIN project_shares s ON s.project_id = p.id AND s.user_id = ?
		ORDER BY p.updated_at DESC`, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("list projects for user: %w", err)
	}
	defer rows.Close()

	out := []*ProjectAccess{}
	for rows.Next() {
		var pa ProjectAccess
		var role string
		if err := rows.Scan(&pa.Project.ID, &pa.Project.WorkspaceID, &pa.Project.Slug, &pa.Project.Name,
			&pa.Project.Description, &pa.Project.CreatedAt, &pa.Project.UpdatedAt,
			&pa.WorkspaceSlug, &pa.WorkspaceName, &role); err != nil {
			return nil, err
		}
		pa.Role = Role(role)
		out = append(out, &pa)
	}
	return out, rows.Err()
}

// ProjectAccess bundles a workspace project with its workspace identity and
// the querying user's effective role — the payload shape the dashboard and
// project views consume.
type ProjectAccess struct {
	Project       `json:"inline"`
	WorkspaceSlug string `json:"workspaceSlug"`
	WorkspaceName string `json:"workspaceName"`
	Role          Role   `json:"role"`
}

func validWorkspaceRole(r Role) bool {
	return r == RoleOwner || r == RoleAdmin || r == RoleMember
}

// stringComma prefixes each column of a comma-separated list, for joins.
func stringComma(cols, prefix string) string {
	out := make([]byte, 0, len(cols)+len(prefix))
	for i := 0; i < len(cols); i++ {
		c := cols[i]
		if c == ' ' || c == ',' {
			continue
		}
		// rebuild: split on ", " boundaries crudely — columns lists use ", "
		out = append(out, c)
	}
	_ = out
	// Simpler and unambiguous: manual split.
	var b []byte
	start := 0
	src := cols
	for i := 0; i <= len(src); i++ {
		if i == len(src) || src[i] == ',' {
			col := trimSpace(src[start:i])
			if len(b) > 0 {
				b = append(b, ',', ' ')
			}
			b = append(b, prefix...)
			b = append(b, col...)
			start = i + 1
		}
	}
	return string(b)
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n') {
		j--
	}
	return s[i:j]
}

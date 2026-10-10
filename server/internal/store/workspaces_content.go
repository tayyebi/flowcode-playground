package store

import (
	"context"
	"database/sql"
	"fmt"
)

// This file gives every workspace-scoped CRUD method the same name as its
// legacy single-tenant twin, with a `ws` prefix on the tables it touches.
// The legacy methods (CreateProject, ListFiles, ...) keep serving only the
// break-glass admin path against the phase-A tables; all authenticated SaaS
// traffic goes through these.
//
// Table mapping: projects→ws_projects, files→ws_files, versions→ws_versions,
// version_files→ws_version_files, deployments→ws_deployments,
// triggers→ws_triggers, executions→ws_executions, kv_store→ws_kv_store.

const wsProjectColumns = "id, workspace_id, COALESCE(created_by, 0), slug, name, description, created_at, updated_at"

// WSProject is a project inside a workspace — the SaaS-phase replacement for
// Project, which stays as the legacy/admin shape.
type WSProject struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspaceId"`
	CreatedBy   int64  `json:"createdBy"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func scanWSProject(row rowScanner) (*WSProject, error) {
	var p WSProject
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.CreatedBy, &p.Slug, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateWSProject inserts a project into a workspace, deriving a slug unique
// *within that workspace* from the name (Cloudflare-style: two accounts may
// both have "api").
func (s *Store) CreateWSProject(ctx context.Context, workspaceID, createdBy int64, name, description string) (*WSProject, error) {
	base := Slugify(name)
	if base == "" {
		base = "project"
	}
	for attempt := 0; attempt < 100; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO ws_projects (workspace_id, created_by, slug, name, description) VALUES (?, ?, ?, ?, ?)`,
			workspaceID, createdBy, slug, name, description)
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return nil, fmt.Errorf("insert ws project: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		return s.GetWSProject(ctx, id)
	}
	return nil, fmt.Errorf("could not find a unique slug for %q", name)
}

func (s *Store) GetWSProject(ctx context.Context, id int64) (*WSProject, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+wsProjectColumns+` FROM ws_projects WHERE id = ?`, id)
	return scanWSProject(row)
}

func (s *Store) UpdateWSProject(ctx context.Context, id int64, name, description *string) (*WSProject, error) {
	if name != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ws_projects SET name = ?, updated_at = datetime('now') WHERE id = ?`, *name, id); err != nil {
			return nil, fmt.Errorf("update ws project name: %w", err)
		}
	}
	if description != nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ws_projects SET description = ?, updated_at = datetime('now') WHERE id = ?`, *description, id); err != nil {
			return nil, fmt.Errorf("update ws project description: %w", err)
		}
	}
	return s.GetWSProject(ctx, id)
}

func (s *Store) DeleteWSProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ws_projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete ws project: %w", err)
	}
	return nil
}

func (s *Store) touchWSProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ws_projects SET updated_at = datetime('now') WHERE id = ?`, id)
	return err
}

// Files -------------------------------------------------------------------

func (s *Store) ListWSFiles(ctx context.Context, projectID int64) ([]*File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM ws_files WHERE project_id = ? ORDER BY position, name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws files: %w", err)
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

func (s *Store) GetWSFile(ctx context.Context, projectID int64, name string) (*File, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM ws_files WHERE project_id = ? AND name = ?`, projectID, name)
	return scanFile(row)
}

func (s *Store) GetWSFileByID(ctx context.Context, id int64) (*File, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM ws_files WHERE id = ?`, id)
	return scanFile(row)
}

func (s *Store) UpsertWSFile(ctx context.Context, projectID int64, name, content string) (*File, error) {
	existing, err := s.GetWSFile(ctx, projectID, name)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE ws_files SET content = ?, updated_at = datetime('now') WHERE id = ?`, content, existing.ID); err != nil {
			return nil, fmt.Errorf("update ws file: %w", err)
		}
		s.touchWSProject(ctx, projectID)
		return s.GetWSFileByID(ctx, existing.ID)
	}
	if err != ErrNotFound {
		return nil, fmt.Errorf("get ws file: %w", err)
	}

	var nextPos int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position) + 1, 0) FROM ws_files WHERE project_id = ?`, projectID).Scan(&nextPos); err != nil {
		return nil, fmt.Errorf("compute next position: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ws_files (project_id, name, content, position) VALUES (?, ?, ?, ?)`,
		projectID, name, content, nextPos)
	if err != nil {
		return nil, fmt.Errorf("insert ws file: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	s.touchWSProject(ctx, projectID)
	return s.GetWSFileByID(ctx, id)
}

func (s *Store) DeleteWSFile(ctx context.Context, projectID int64, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ws_files WHERE project_id = ? AND name = ?`, projectID, name); err != nil {
		return fmt.Errorf("delete ws file: %w", err)
	}
	return s.touchWSProject(ctx, projectID)
}

// Versions ------------------------------------------------------------------

const wsVersionColumns = "id, project_id, number, label, created_at"

func scanWSVersion(row rowScanner) (*Version, error) {
	var v Version
	if err := row.Scan(&v.ID, &v.ProjectID, &v.Number, &v.Label, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) CreateWSVersion(ctx context.Context, projectID int64, label string) (*Version, error) {
	files, err := s.ListWSFiles(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws files to snapshot: %w", err)
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
		`SELECT COALESCE(MAX(number) + 1, 1) FROM ws_versions WHERE project_id = ?`, projectID).Scan(&nextNumber); err != nil {
		return nil, fmt.Errorf("compute next version number: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO ws_versions (project_id, number, label) VALUES (?, ?, ?)`, projectID, nextNumber, label)
	if err != nil {
		return nil, fmt.Errorf("insert ws version: %w", err)
	}
	versionID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}

	for _, f := range files {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ws_version_files (version_id, name, content) VALUES (?, ?, ?)`,
			versionID, f.Name, f.Content); err != nil {
			return nil, fmt.Errorf("insert ws version file %s: %w", f.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetWSVersion(ctx, versionID)
}

func (s *Store) GetWSVersion(ctx context.Context, id int64) (*Version, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+wsVersionColumns+` FROM ws_versions WHERE id = ?`, id)
	return scanWSVersion(row)
}

func (s *Store) GetWSVersionByNumber(ctx context.Context, projectID int64, number int) (*Version, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+wsVersionColumns+` FROM ws_versions WHERE project_id = ? AND number = ?`, projectID, number)
	return scanWSVersion(row)
}

func (s *Store) ListWSVersions(ctx context.Context, projectID int64) ([]*Version, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+wsVersionColumns+` FROM ws_versions WHERE project_id = ? ORDER BY number DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws versions: %w", err)
	}
	defer rows.Close()

	versions := []*Version{}
	for rows.Next() {
		v, err := scanWSVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *Store) GetWSVersionFileContent(ctx context.Context, versionID int64, name string) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx,
		`SELECT content FROM ws_version_files WHERE version_id = ? AND name = ?`, versionID, name).Scan(&content)
	if err != nil {
		return "", err
	}
	return content, nil
}

func (s *Store) RestoreWSVersion(ctx context.Context, projectID, versionID int64) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, content FROM ws_version_files WHERE version_id = ? ORDER BY name`, versionID)
	if err != nil {
		return fmt.Errorf("list ws version files: %w", err)
	}
	defer rows.Close()

	type snap struct{ name, content string }
	var snaps []snap
	for rows.Next() {
		var s snap
		if err := rows.Scan(&s.name, &s.content); err != nil {
			return err
		}
		snaps = append(snaps, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range snaps {
		if _, err := s.UpsertWSFile(ctx, projectID, f.name, f.content); err != nil {
			return fmt.Errorf("restore ws file %s: %w", f.name, err)
		}
	}
	return nil
}

// Deployments ---------------------------------------------------------------

const wsDeploymentColumns = `d.id, d.project_id, d.file_id, f.name, d.slug, d.version_id, d.enabled, d.created_at, d.updated_at`

func wsDeploymentQuery(where string) string {
	return `SELECT ` + wsDeploymentColumns + ` FROM ws_deployments d JOIN ws_files f ON f.id = d.file_id WHERE ` + where
}

func (s *Store) CreateWSDeployment(ctx context.Context, projectID, fileID int64, versionID *int64) (*Deployment, error) {
	for attempt := 0; attempt < 10; attempt++ {
		slug, err := randomSlug()
		if err != nil {
			return nil, err
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO ws_deployments (project_id, file_id, slug, version_id) VALUES (?, ?, ?, ?)`,
			projectID, fileID, slug, versionID)
		if err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return nil, fmt.Errorf("insert ws deployment: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		return s.GetWSDeployment(ctx, id)
	}
	return nil, fmt.Errorf("could not generate a unique deployment slug")
}

func (s *Store) GetWSDeployment(ctx context.Context, id int64) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, wsDeploymentQuery("d.id = ?"), id)
	return scanDeployment(row)
}

func (s *Store) GetWSDeploymentBySlug(ctx context.Context, slug string) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, wsDeploymentQuery("d.slug = ?"), slug)
	return scanDeployment(row)
}

func (s *Store) ListWSDeployments(ctx context.Context, projectID int64) ([]*Deployment, error) {
	rows, err := s.db.QueryContext(ctx, wsDeploymentQuery("d.project_id = ?")+" ORDER BY d.created_at DESC", projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws deployments: %w", err)
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

func (s *Store) SetWSDeploymentEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE ws_deployments SET enabled = ?, updated_at = datetime('now') WHERE id = ?`, v, id)
	return err
}

func (s *Store) DeleteWSDeployment(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ws_deployments WHERE id = ?`, id)
	return err
}

// Triggers --------------------------------------------------------------------

const wsTriggerColumns = `t.id, t.project_id, t.file_id, f.name, t.version_id, t.schedule_type,
	t.interval_seconds, t.daily_time_utc, t.enabled, t.next_run_at, t.last_run_at, t.created_at, t.updated_at`

func wsTriggerQuery(where string) string {
	return `SELECT ` + wsTriggerColumns + ` FROM ws_triggers t JOIN ws_files f ON f.id = t.file_id WHERE ` + where
}

// scanTriggerFrom has the identical column layout as legacy scanTrigger, so
// it just delegates.
func scanTriggerFrom(row rowScanner) (*Trigger, error) { return scanTrigger(row) }

func (s *Store) CreateWSTrigger(ctx context.Context, nt NewTrigger) (*Trigger, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ws_triggers (project_id, file_id, version_id, schedule_type, interval_seconds, daily_time_utc, next_run_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nt.ProjectID, nt.FileID, nt.VersionID, nt.ScheduleType, nt.IntervalSeconds, nullIfEmpty(nt.DailyTimeUTC), nt.NextRunAt)
	if err != nil {
		return nil, fmt.Errorf("insert ws trigger: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetWSTrigger(ctx, id)
}

func (s *Store) GetWSTrigger(ctx context.Context, id int64) (*Trigger, error) {
	row := s.db.QueryRowContext(ctx, wsTriggerQuery("t.id = ?"), id)
	return scanTriggerFrom(row)
}

func (s *Store) ListWSTriggers(ctx context.Context, projectID int64) ([]*Trigger, error) {
	rows, err := s.db.QueryContext(ctx, wsTriggerQuery("t.project_id = ?")+" ORDER BY t.created_at DESC", projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws triggers: %w", err)
	}
	defer rows.Close()

	triggers := []*Trigger{}
	for rows.Next() {
		t, err := scanTriggerFrom(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *Store) WSDueTriggers(ctx context.Context, nowUTC string) ([]*Trigger, error) {
	rows, err := s.db.QueryContext(ctx, wsTriggerQuery("t.enabled = 1 AND t.next_run_at <= ?"), nowUTC)
	if err != nil {
		return nil, fmt.Errorf("list ws due triggers: %w", err)
	}
	defer rows.Close()

	triggers := []*Trigger{}
	for rows.Next() {
		t, err := scanTriggerFrom(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *Store) SetWSTriggerEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE ws_triggers SET enabled = ?, updated_at = datetime('now') WHERE id = ?`, v, id)
	return err
}

func (s *Store) RecordWSTriggerRun(ctx context.Context, id int64, lastRunAt, nextRunAt string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ws_triggers SET last_run_at = ?, next_run_at = ?, updated_at = datetime('now') WHERE id = ?`,
		lastRunAt, nextRunAt, id)
	return err
}

func (s *Store) DeleteWSTrigger(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ws_triggers WHERE id = ?`, id)
	return err
}

// Executions ------------------------------------------------------------------

const wsExecutionColumns = `id, project_id, file_id, file_name, version_id, source, deployment_id, trigger_id, actor_id,
	started_at, finished_at, duration_ms, compile_exit_code, compile_stderr, run_exit_code, run_stderr,
	timed_out, truncated, error`

// executionFields flattens a run summary (or outright error) into the
// columns both the legacy and ws execution inserts need. Shared so the two
// tables can never drift in how they record an outcome.
func executionFields(ne NewExecution) (durationMs *int64, compileExit, runExit *int, compileStderr, runStderr string, timedOut, truncated bool, errMsg string) {
	if ne.Summary != nil {
		ce := ne.Summary.CompileExitCode
		compileExit = &ce
		compileStderr = ne.Summary.CompileStderr
		truncated = ne.Summary.Truncated
		timedOut = ne.Summary.CompileTimedOut
		var total int64
		total = ne.Summary.CompileDurationMs
		if ne.Summary.HasRun {
			re := ne.Summary.RunExitCode
			runExit = &re
			runStderr = ne.Summary.RunStderr
			timedOut = timedOut || ne.Summary.RunTimedOut
			total += ne.Summary.RunDurationMs
		}
		durationMs = &total
	}
	if ne.Err != nil {
		errMsg = ne.Err.Error()
	}
	return
}

func (s *Store) RecordWSSExecution(ctx context.Context, ne NewExecution, actorID *int64) (*Execution, error) {
	durationMs, compileExit, runExit, compileStderr, runStderr, timedOut, truncated, errMsg := executionFields(ne)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ws_executions (project_id, file_id, file_name, version_id, source, deployment_id, trigger_id, actor_id,
				started_at, finished_at, duration_ms, compile_exit_code, compile_stderr, run_exit_code, run_stderr,
				timed_out, truncated, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ne.ProjectID, ne.FileID, ne.FileName, ne.VersionID, ne.Source, ne.DeploymentID, ne.TriggerID, actorID,
		ne.StartedAt, ne.FinishedAt, durationMs, compileExit, compileStderr, runExit, runStderr,
		boolToInt(timedOut), boolToInt(truncated), nullIfEmpty(errMsg))
	if err != nil {
		return nil, fmt.Errorf("insert ws execution: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetWSExecution(ctx, id)
}

func scanWSExecution(row rowScanner) (*Execution, error) {
	var e Execution
	var fileID, versionID, deploymentID, triggerID, actorID, durationMs sql.NullInt64
	var compileExit, runExit sql.NullInt64
	var finishedAt, compileStderr, runStderr, errMsg sql.NullString
	var timedOut, truncated int

	if err := row.Scan(&e.ID, &e.ProjectID, &fileID, &e.FileName, &versionID, &e.Source, &deploymentID, &triggerID, &actorID,
		&e.StartedAt, &finishedAt, &durationMs, &compileExit, &compileStderr, &runExit, &runStderr,
		&timedOut, &truncated, &errMsg); err != nil {
		return nil, err
	}
	if fileID.Valid {
		v := fileID.Int64
		e.FileID = &v
	}
	if versionID.Valid {
		v := versionID.Int64
		e.VersionID = &v
	}
	if deploymentID.Valid {
		v := deploymentID.Int64
		e.DeploymentID = &v
	}
	if triggerID.Valid {
		v := triggerID.Int64
		e.TriggerID = &v
	}
	if actorID.Valid {
		v := actorID.Int64
		e.ActorID = &v
	}
	if durationMs.Valid {
		v := durationMs.Int64
		e.DurationMs = &v
	}
	if compileExit.Valid {
		v := int(compileExit.Int64)
		e.CompileExitCode = &v
	}
	if runExit.Valid {
		v := int(runExit.Int64)
		e.RunExitCode = &v
	}
	e.FinishedAt = finishedAt.String
	e.CompileStderr = compileStderr.String
	e.RunStderr = runStderr.String
	e.Error = errMsg.String
	e.TimedOut = timedOut != 0
	e.Truncated = truncated != 0
	return &e, nil
}

func (s *Store) GetWSExecution(ctx context.Context, id int64) (*Execution, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+wsExecutionColumns+` FROM ws_executions WHERE id = ?`, id)
	return scanWSExecution(row)
}

func (s *Store) ListWSExecutions(ctx context.Context, projectID int64, limit int, beforeID int64) ([]*Execution, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+wsExecutionColumns+` FROM ws_executions WHERE project_id = ? AND id < ? ORDER BY id DESC LIMIT ?`,
			projectID, beforeID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+wsExecutionColumns+` FROM ws_executions WHERE project_id = ? ORDER BY id DESC LIMIT ?`,
			projectID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list ws executions: %w", err)
	}
	defer rows.Close()

	executions := []*Execution{}
	for rows.Next() {
		e, err := scanWSExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, e)
	}
	return executions, rows.Err()
}

func (s *Store) PruneWSExecutions(ctx context.Context, keepPerProject int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM ws_executions WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY project_id ORDER BY id DESC) AS rn
				FROM ws_executions
			) WHERE rn > ?
		)`, keepPerProject)
	if err != nil {
		return 0, fmt.Errorf("prune ws executions: %w", err)
	}
	return res.RowsAffected()
}

// KV -------------------------------------------------------------------------

func (s *Store) UpsertWSKV(ctx context.Context, projectID int64, key, value string, sourceExecutionID *int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ws_kv_store (project_id, key, value, updated_at, source_execution_id)
		VALUES (?, ?, ?, datetime('now'), ?)
		ON CONFLICT(project_id, key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			source_execution_id = excluded.source_execution_id`,
		projectID, key, value, sourceExecutionID)
	if err != nil {
		return fmt.Errorf("upsert ws kv: %w", err)
	}
	return nil
}

func (s *Store) ListWSKV(ctx context.Context, projectID int64) ([]*KVEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, key, value, updated_at, source_execution_id FROM ws_kv_store WHERE project_id = ? ORDER BY key`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("list ws kv: %w", err)
	}
	defer rows.Close()

	entries := []*KVEntry{}
	for rows.Next() {
		var e KVEntry
		var sourceExecID *int64
		if err := rows.Scan(&e.ProjectID, &e.Key, &e.Value, &e.UpdatedAt, &sourceExecID); err != nil {
			return nil, err
		}
		e.SourceExecutionID = sourceExecID
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

func (s *Store) DeleteWSKV(ctx context.Context, projectID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ws_kv_store WHERE project_id = ? AND key = ?`, projectID, key)
	return err
}

// AuditEntry is one ws execution joined with its project and actor, for the
// global admin executions audit.
type AuditEntry struct {
	Execution
	ProjectName string `json:"projectName"`
	ActorEmail  string `json:"actorEmail,omitempty"`
}

// ListWSAudit returns the newest executions across every workspace.
func (s *Store) ListWSAudit(ctx context.Context, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.project_id, e.file_id, e.file_name, e.version_id, e.source, e.deployment_id, e.trigger_id, e.actor_id,
		       e.started_at, e.finished_at, e.duration_ms, e.compile_exit_code, e.compile_stderr, e.run_exit_code, e.run_stderr,
		       e.timed_out, e.truncated, e.error,
		       COALESCE(p.name, ''), COALESCE(u.email, '')
		FROM ws_executions e
		LEFT JOIN ws_projects p ON p.id = e.project_id
		LEFT JOIN users u ON u.id = e.actor_id
		ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list ws audit: %w", err)
	}
	defer rows.Close()

	out := []*AuditEntry{}
	for rows.Next() {
		a := &AuditEntry{}
		var fileID, versionID, deploymentID, triggerID, durationMs sql.NullInt64
		var compileExit, runExit sql.NullInt64
		var finishedAt, compileStderr, runStderr, errMsg sql.NullString
		var timedOut, truncated int
		if err := rows.Scan(&a.ID, &a.ProjectID, &fileID, &a.FileName, &versionID, &a.Source, &deploymentID, &triggerID, &a.ActorID,
			&a.StartedAt, &finishedAt, &durationMs, &compileExit, &compileStderr, &runExit, &runStderr,
			&timedOut, &truncated, &errMsg, &a.ProjectName, &a.ActorEmail); err != nil {
			return nil, err
		}
		if fileID.Valid {
			v := fileID.Int64
			a.FileID = &v
		}
		if versionID.Valid {
			v := versionID.Int64
			a.VersionID = &v
		}
		if deploymentID.Valid {
			v := deploymentID.Int64
			a.DeploymentID = &v
		}
		if triggerID.Valid {
			v := triggerID.Int64
			a.TriggerID = &v
		}
		if durationMs.Valid {
			v := durationMs.Int64
			a.DurationMs = &v
		}
		if compileExit.Valid {
			v := int(compileExit.Int64)
			a.CompileExitCode = &v
		}
		if runExit.Valid {
			v := int(runExit.Int64)
			a.RunExitCode = &v
		}
		a.FinishedAt = finishedAt.String
		a.CompileStderr = compileStderr.String
		a.RunStderr = runStderr.String
		a.Error = errMsg.String
		a.TimedOut = timedOut != 0
		a.Truncated = truncated != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Execution is one recorded run of a project file, from any of the three
// sources that can trigger one: a manual project-run, a deployment request,
// or a scheduled trigger. ActorID is set for workspace executions (the user
// the run was attributed to); legacy single-tenant rows leave it NULL.
type Execution struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"projectId"`
	FileID          *int64 `json:"fileId,omitempty"`
	FileName        string `json:"fileName"`
	VersionID       *int64 `json:"versionId,omitempty"`
	Source          string `json:"source"` // 'project-run' | 'deployment' | 'trigger'
	DeploymentID    *int64 `json:"deploymentId,omitempty"`
	TriggerID       *int64 `json:"triggerId,omitempty"`
	ActorID         *int64 `json:"actorId,omitempty"`
	StartedAt       string `json:"startedAt"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	DurationMs      *int64 `json:"durationMs,omitempty"`
	CompileExitCode *int   `json:"compileExitCode,omitempty"`
	CompileStderr   string `json:"compileStderr,omitempty"`
	RunExitCode     *int   `json:"runExitCode,omitempty"`
	RunStderr       string `json:"runStderr,omitempty"`
	TimedOut        bool   `json:"timedOut"`
	Truncated       bool   `json:"truncated"`
	Error           string `json:"error,omitempty"`
}

// RunSummary is the flattened outcome of one engine run — the parts the
// executions table records. The engine maps its own Result into this so the
// store layer does not depend on it.
type RunSummary struct {
	CompileExitCode   int
	CompileStderr     string
	CompileTimedOut   bool
	CompileDurationMs int64
	HasRun            bool
	RunExitCode       int
	RunStderr         string
	RunTimedOut       bool
	RunDurationMs     int64
	Truncated         bool
}

// NewExecution is what a caller records after a run returns (or fails
// outright, in which case Summary is nil and Err carries the reason).
type NewExecution struct {
	ProjectID    int64
	FileID       *int64
	FileName     string
	VersionID    *int64
	Source       string
	DeploymentID *int64
	TriggerID    *int64
	StartedAt    string
	FinishedAt   string
	Summary      *RunSummary
	Err          error
}

func (s *Store) RecordExecution(ctx context.Context, ne NewExecution) (*Execution, error) {
	durationMs, compileExit, runExit, compileStderr, runStderr, timedOut, truncated, errMsg := executionFields(ne)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO executions (project_id, file_id, file_name, version_id, source, deployment_id, trigger_id,
			started_at, finished_at, duration_ms, compile_exit_code, compile_stderr, run_exit_code, run_stderr,
			timed_out, truncated, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ne.ProjectID, ne.FileID, ne.FileName, ne.VersionID, ne.Source, ne.DeploymentID, ne.TriggerID,
		ne.StartedAt, ne.FinishedAt, durationMs, compileExit, compileStderr, runExit, runStderr,
		boolToInt(timedOut), boolToInt(truncated), nullIfEmpty(errMsg))
	if err != nil {
		return nil, fmt.Errorf("insert execution: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetExecution(ctx, id)
}

const executionColumns = `id, project_id, file_id, file_name, version_id, source, deployment_id, trigger_id,
	started_at, finished_at, duration_ms, compile_exit_code, compile_stderr, run_exit_code, run_stderr,
	timed_out, truncated, error`

func scanExecution(row rowScanner) (*Execution, error) {
	var e Execution
	var fileID, versionID, deploymentID, triggerID, durationMs sql.NullInt64
	var compileExit, runExit sql.NullInt64
	var finishedAt, compileStderr, runStderr, errMsg sql.NullString
	var timedOut, truncated int

	if err := row.Scan(&e.ID, &e.ProjectID, &fileID, &e.FileName, &versionID, &e.Source, &deploymentID, &triggerID,
		&e.StartedAt, &finishedAt, &durationMs, &compileExit, &compileStderr, &runExit, &runStderr,
		&timedOut, &truncated, &errMsg); err != nil {
		return nil, err
	}
	if fileID.Valid {
		e.FileID = &fileID.Int64
	}
	if versionID.Valid {
		e.VersionID = &versionID.Int64
	}
	if deploymentID.Valid {
		e.DeploymentID = &deploymentID.Int64
	}
	if triggerID.Valid {
		e.TriggerID = &triggerID.Int64
	}
	if durationMs.Valid {
		e.DurationMs = &durationMs.Int64
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

func (s *Store) GetExecution(ctx context.Context, id int64) (*Execution, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+executionColumns+` FROM executions WHERE id = ?`, id)
	return scanExecution(row)
}

// ListExecutions pages through a project's history, newest first. Pass 0 for
// beforeID to get the first page.
func (s *Store) ListExecutions(ctx context.Context, projectID int64, limit int, beforeID int64) ([]*Execution, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+executionColumns+` FROM executions WHERE project_id = ? AND id < ? ORDER BY id DESC LIMIT ?`,
			projectID, beforeID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+executionColumns+` FROM executions WHERE project_id = ? ORDER BY id DESC LIMIT ?`,
			projectID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()

	executions := []*Execution{}
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, e)
	}
	return executions, rows.Err()
}

// PruneExecutions deletes all but the most recent keepPerProject rows for
// every project, so a fast-firing trigger doesn't grow this table forever.
func (s *Store) PruneExecutions(ctx context.Context, keepPerProject int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM executions WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY project_id ORDER BY id DESC) AS rn
				FROM executions
			) WHERE rn > ?
		)`, keepPerProject)
	if err != nil {
		return 0, fmt.Errorf("prune executions: %w", err)
	}
	return res.RowsAffected()
}

// RecordRunWithKV records an execution row and, best-effort, any `store set`
// calls it produced by parsing the run trace. Shared by project runs,
// deployments, and triggers so the bookkeeping is identical no matter which
// path invoked the engine.
func (s *Store) RecordRunWithKV(ctx context.Context, ne NewExecution) (*Execution, error) {
	exec, err := s.RecordExecution(ctx, ne)
	if err != nil {
		return nil, err
	}
	if ne.Summary != nil && ne.Summary.HasRun {
		for k, v := range ExtractStoreSets(ne.Summary.RunStderr) {
			s.UpsertKV(ctx, ne.ProjectID, k, v, &exec.ID)
		}
	}
	return exec, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

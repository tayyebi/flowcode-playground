package store

import (
	"context"
	"fmt"
	"regexp"
)

// KVEntry is one row of a project's best-effort, write-side key/value log.
//
// Phase A limitation (see the plan doc): FlowCode's `store` builtin has no
// confirmed read-back mechanism, so this is populated by parsing `store set`
// calls out of the run trace after the fact. It is a debug/audit log a human
// can inspect from the dashboard, not a working key-value API a running
// script can read from — do not present it as the latter in the UI.
type KVEntry struct {
	ProjectID         int64  `json:"projectId"`
	Key               string `json:"key"`
	Value             string `json:"value"`
	UpdatedAt         string `json:"updatedAt"`
	SourceExecutionID *int64 `json:"sourceExecutionId,omitempty"`
}

func (s *Store) UpsertKV(ctx context.Context, projectID int64, key, value string, sourceExecutionID *int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kv_store (project_id, key, value, updated_at, source_execution_id)
		VALUES (?, ?, ?, datetime('now'), ?)
		ON CONFLICT(project_id, key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			source_execution_id = excluded.source_execution_id`,
		projectID, key, value, sourceExecutionID)
	if err != nil {
		return fmt.Errorf("upsert kv: %w", err)
	}
	return nil
}

func (s *Store) ListKV(ctx context.Context, projectID int64) ([]*KVEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, key, value, updated_at, source_execution_id FROM kv_store WHERE project_id = ? ORDER BY key`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("list kv: %w", err)
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

func (s *Store) DeleteKV(ctx context.Context, projectID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv_store WHERE project_id = ? AND key = ?`, projectID, key)
	return err
}

// storeSetRe is a best-effort match against fcplay's DEBUG trace for a
// `store set` call. fcplay runs the VM at DEBUG log level specifically so
// every builtin plugin invocation is logged (README.md), but the exact line
// format for a plugin call has not been confirmed against a real build in
// this environment — this pattern is a starting point and MUST be checked
// against actual fcplay output (`key = "..."` / `value = "..."` are the
// param names used in FlowCode's own `store set` syntax) and adjusted before
// this is relied on for anything beyond a best-effort debug log.
var storeSetRe = regexp.MustCompile(`(?i)store\s+set.*?key\s*=\s*"([^"]*)".*?value\s*=\s*"([^"]*)"`)

// ExtractStoreSets pulls `store set key="..." value="..."` calls out of a run
// trace. It returns no error on zero matches — an empty result is a normal,
// expected outcome for a workflow that never calls `store set`.
func ExtractStoreSets(trace string) map[string]string {
	writes := map[string]string{}
	for _, m := range storeSetRe.FindAllStringSubmatch(trace, -1) {
		writes[m[1]] = m[2]
	}
	return writes
}

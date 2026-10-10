package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

/* ------------------------------------------------------------------ */
/* system_settings: one JSON blob per settings group                   */
/* ------------------------------------------------------------------ */

// GetSetting returns the raw JSON stored under key, or "" when absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return v, nil
}

// PutSetting stores a JSON blob under key. updatedBy may be 0 (system);
// the users FK then stores NULL.
func (s *Store) PutSetting(ctx context.Context, key, value string, updatedBy int64) error {
	var by any
	if updatedBy > 0 {
		by = updatedBy
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value, updated_at, updated_by) VALUES (?, ?, datetime('now'), ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now'), updated_by = excluded.updated_by`,
		key, value, by)
	if err != nil {
		return fmt.Errorf("put setting %s: %w", key, err)
	}
	return nil
}

// GetSettingJSON decodes the blob under key into v; a missing key leaves v
// untouched so callers can pre-fill defaults.
func (s *Store) GetSettingJSON(ctx context.Context, key string, v any) error {
	raw, err := s.GetSetting(ctx, key)
	if err != nil || raw == "" {
		return err
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("setting %s is not valid JSON: %w", key, err)
	}
	return nil
}

// Internal (__) keys are never shown or edited in the admin UI.
func IsInternalSetting(key string) bool {
	return len(key) > 2 && key[:2] == "__"
}

/* ------------------------------------------------------------------ */
/* app_calls: bridge invocation records (mail/http only; logger→Loki)   */
/* ------------------------------------------------------------------ */

// AppCall is one app invocation recorded from a run.
type AppCall struct {
	ID          int64  `json:"id"`
	ExecutionID int64  `json:"executionId"`
	UserID      *int64 `json:"userId,omitempty"`
	App         string `json:"app"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Request     string `json:"request,omitempty"`
	Response    string `json:"response,omitempty"`
	DurationMs  int64  `json:"durationMs"`
	CreatedAt   string `json:"createdAt"`
}

// RecordAppCalls persists the in-memory call list of a finished run against
// its execution row. Logger entries are skipped by the caller — they live in
// Loki, not here.
func (s *Store) RecordAppCalls(ctx context.Context, calls []*AppCall) error {
	for _, c := range calls {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO app_calls (execution_id, user_id, app, name, status, request, response, duration_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ExecutionID, c.UserID, c.App, c.Name, c.Status, c.Request, c.Response, c.DurationMs); err != nil {
			return fmt.Errorf("record app call: %w", err)
		}
	}
	return nil
}

// AppCallFilter narrows the admin Logs pages.
type AppCallFilter struct {
	App    string // "" = all
	UserID int64  // 0 = all
	Limit  int
}

func (s *Store) ListAppCalls(ctx context.Context, f AppCallFilter) ([]*AppCall, error) {
	q := `SELECT c.id, c.execution_id, c.user_id, c.app, c.name, c.status, c.request, c.response, c.duration_ms, c.created_at
	      FROM app_calls c`
	where, args := "", []any{}
	if f.App != "" {
		where += " WHERE c.app = ?"
		args = append(args, f.App)
	}
	if f.UserID != 0 {
		if where == "" {
			where = " WHERE"
		} else {
			where += " AND"
		}
		where += " c.user_id = ?"
		args = append(args, f.UserID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += where + ` ORDER BY c.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list app calls: %w", err)
	}
	defer rows.Close()

	out := []*AppCall{}
	for rows.Next() {
		c := &AppCall{}
		if err := rows.Scan(&c.ID, &c.ExecutionID, &c.UserID, &c.App, &c.Name, &c.Status, &c.Request, &c.Response, &c.DurationMs, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListExecutionAppCalls returns the transcript rows for one execution.
func (s *Store) ListExecutionAppCalls(ctx context.Context, executionID int64) ([]*AppCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, execution_id, user_id, app, name, status, request, response, duration_ms, created_at
		FROM app_calls WHERE execution_id = ? ORDER BY id`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list execution app calls: %w", err)
	}
	defer rows.Close()

	out := []*AppCall{}
	for rows.Next() {
		c := &AppCall{}
		if err := rows.Scan(&c.ID, &c.ExecutionID, &c.UserID, &c.App, &c.Name, &c.Status, &c.Request, &c.Response, &c.DurationMs, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------------ */
/* app_quotas: per-user per-app overrides                              */
/* ------------------------------------------------------------------ */

// AppQuota is one user's override for one app. CallsPerMinute == nil means
// "use the app default"; Enabled=false disables the app for this user.
type AppQuota struct {
	UserID         int64  `json:"userId"`
	UserEmail      string `json:"userEmail,omitempty"`
	App            string `json:"app"`
	CallsPerMinute *int   `json:"callsPerMinute"`
	Enabled        bool   `json:"enabled"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
}

func (s *Store) UpsertAppQuota(ctx context.Context, q AppQuota) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_quotas (user_id, app, calls_per_minute, enabled) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, app) DO UPDATE SET calls_per_minute = excluded.calls_per_minute, enabled = excluded.enabled`,
		q.UserID, q.App, q.CallsPerMinute, enabledInt(q.Enabled))
	if err != nil {
		return fmt.Errorf("upsert app quota: %w", err)
	}
	return nil
}

func (s *Store) DeleteAppQuota(ctx context.Context, userID int64, app string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_quotas WHERE user_id = ? AND app = ?`, userID, app)
	return err
}

func (s *Store) ListAppQuotas(ctx context.Context, app string) ([]*AppQuota, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT q.user_id, COALESCE(u.email, ''), q.app, q.calls_per_minute, q.enabled
		FROM app_quotas q LEFT JOIN users u ON u.id = q.user_id
		WHERE q.app = ? ORDER BY u.email`, app)
	if err != nil {
		return nil, fmt.Errorf("list app quotas: %w", err)
	}
	defer rows.Close()

	out := []*AppQuota{}
	for rows.Next() {
		q := &AppQuota{}
		var cpm sql.NullInt64
		var enabled int
		if err := rows.Scan(&q.UserID, &q.UserEmail, &q.App, &cpm, &enabled); err != nil {
			return nil, err
		}
		if cpm.Valid {
			v := int(cpm.Int64)
			q.CallsPerMinute = &v
		}
		q.Enabled = enabled != 0
		out = append(out, q)
	}
	return out, rows.Err()
}

// EffectiveQuota resolves the limit for (user, app): the user's override when
// present, otherwise def (the app default from settings).
func (s *Store) EffectiveQuota(ctx context.Context, userID int64, app string, def int) (int, bool, error) {
	var cpm sql.NullInt64
	var enabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT calls_per_minute, enabled FROM app_quotas WHERE user_id = ? AND app = ?`, userID, app).
		Scan(&cpm, &enabled)
	if err == sql.ErrNoRows {
		return def, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	limit := def
	if cpm.Valid && cpm.Int64 >= 0 {
		limit = int(cpm.Int64)
	}
	return limit, enabled != 0, nil
}

func enabledInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

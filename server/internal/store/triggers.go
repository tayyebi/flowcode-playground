package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Trigger runs a project file on a schedule. Deliberately not full cron
// syntax — 'interval' (every N seconds) or 'daily' (HH:MM UTC) — mirroring
// Apps Script's own simplified trigger UI. next_run_at is computed by
// package scheduler, not here; this file is pure persistence.
type Trigger struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"projectId"`
	FileID          int64  `json:"fileId"`
	FileName        string `json:"fileName"`
	VersionID       *int64 `json:"versionId,omitempty"`
	ScheduleType    string `json:"scheduleType"`
	IntervalSeconds *int   `json:"intervalSeconds,omitempty"`
	DailyTimeUTC    string `json:"dailyTimeUtc,omitempty"`
	Enabled         bool   `json:"enabled"`
	NextRunAt       string `json:"nextRunAt"`
	LastRunAt       string `json:"lastRunAt,omitempty"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

const triggerColumns = `t.id, t.project_id, t.file_id, f.name, t.version_id, t.schedule_type,
	t.interval_seconds, t.daily_time_utc, t.enabled, t.next_run_at, t.last_run_at, t.created_at, t.updated_at`

func triggerQuery(where string) string {
	return `SELECT ` + triggerColumns + ` FROM triggers t JOIN files f ON f.id = t.file_id WHERE ` + where
}

func scanTrigger(row rowScanner) (*Trigger, error) {
	var t Trigger
	var enabled int
	var versionID, intervalSeconds sql.NullInt64
	var dailyTime, lastRunAt sql.NullString
	if err := row.Scan(&t.ID, &t.ProjectID, &t.FileID, &t.FileName, &versionID, &t.ScheduleType,
		&intervalSeconds, &dailyTime, &enabled, &t.NextRunAt, &lastRunAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	if versionID.Valid {
		t.VersionID = &versionID.Int64
	}
	if intervalSeconds.Valid {
		v := int(intervalSeconds.Int64)
		t.IntervalSeconds = &v
	}
	t.DailyTimeUTC = dailyTime.String
	t.LastRunAt = lastRunAt.String
	return &t, nil
}

type NewTrigger struct {
	ProjectID       int64
	FileID          int64
	VersionID       *int64
	ScheduleType    string
	IntervalSeconds *int
	DailyTimeUTC    string
	NextRunAt       string
}

func (s *Store) CreateTrigger(ctx context.Context, nt NewTrigger) (*Trigger, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO triggers (project_id, file_id, version_id, schedule_type, interval_seconds, daily_time_utc, next_run_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nt.ProjectID, nt.FileID, nt.VersionID, nt.ScheduleType, nt.IntervalSeconds, nullIfEmpty(nt.DailyTimeUTC), nt.NextRunAt)
	if err != nil {
		return nil, fmt.Errorf("insert trigger: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetTrigger(ctx, id)
}

func (s *Store) GetTrigger(ctx context.Context, id int64) (*Trigger, error) {
	row := s.db.QueryRowContext(ctx, triggerQuery("t.id = ?"), id)
	return scanTrigger(row)
}

func (s *Store) ListTriggers(ctx context.Context, projectID int64) ([]*Trigger, error) {
	rows, err := s.db.QueryContext(ctx, triggerQuery("t.project_id = ?")+" ORDER BY t.created_at DESC", projectID)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()

	triggers := []*Trigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

// DueTriggers returns enabled triggers whose next_run_at has passed, for the
// scheduler to fire.
func (s *Store) DueTriggers(ctx context.Context, nowUTC string) ([]*Trigger, error) {
	rows, err := s.db.QueryContext(ctx,
		triggerQuery("t.enabled = 1 AND t.next_run_at <= ?"), nowUTC)
	if err != nil {
		return nil, fmt.Errorf("list due triggers: %w", err)
	}
	defer rows.Close()

	triggers := []*Trigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *Store) SetTriggerEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE triggers SET enabled = ?, updated_at = datetime('now') WHERE id = ?`, v, id)
	return err
}

// RecordTriggerRun updates last_run_at and the freshly computed next_run_at
// after the scheduler fires a trigger.
func (s *Store) RecordTriggerRun(ctx context.Context, id int64, lastRunAt, nextRunAt string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE triggers SET last_run_at = ?, next_run_at = ?, updated_at = datetime('now') WHERE id = ?`,
		lastRunAt, nextRunAt, id)
	return err
}

func (s *Store) DeleteTrigger(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, id)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

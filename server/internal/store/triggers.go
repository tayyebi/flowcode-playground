package store

import (
	"database/sql"
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

func scanTrigger(row rowScanner) (*Trigger, error) {
	var t Trigger
	var versionID, intervalSeconds sql.NullInt64
	var dailyTime, lastRunAt sql.NullString
	if err := row.Scan(&t.ID, &t.ProjectID, &t.FileID, &t.FileName, &versionID, &t.ScheduleType,
		&intervalSeconds, &dailyTime, &t.Enabled, &t.NextRunAt, &lastRunAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
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

// NewTrigger is the insert payload for CreateWSTrigger.
type NewTrigger struct {
	ProjectID       int64
	FileID          int64
	VersionID       *int64
	ScheduleType    string
	IntervalSeconds *int
	DailyTimeUTC    string
	NextRunAt       string
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

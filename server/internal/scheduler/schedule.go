// Package scheduler computes next-run times for time-driven triggers and
// runs an in-process ticker that fires the ones that are due.
//
// Deliberately not full cron syntax: only 'interval' (every N seconds) and
// 'daily' (HH:MM UTC), mirroring Apps Script's own simplified trigger UI. A
// pure-Go cron library (e.g. robfig/cron/v3) would be the natural upgrade if
// day-of-week/month expressions are ever wanted — not needed for this scope.
package scheduler

import (
	"fmt"
	"time"
)

const timeLayout = time.RFC3339

// NextRun computes the next time a trigger should fire, given now.
//
// For 'interval', it advances by intervalSeconds repeatedly until the result
// is strictly after now — this *skips* any runs missed while the scheduler
// wasn't ticking, rather than bursting through a backlog, mirroring how Apps
// Script's own triggers behave when the host was unavailable.
//
// For 'daily', it returns the next occurrence of dailyTimeUTC (HH:MM),
// tomorrow if today's time has already passed.
func NextRun(now time.Time, scheduleType string, intervalSeconds int, dailyTimeUTC string) (time.Time, error) {
	now = now.UTC()
	switch scheduleType {
	case "interval":
		if intervalSeconds <= 0 {
			return time.Time{}, fmt.Errorf("interval schedule needs a positive interval_seconds")
		}
		next := now.Add(time.Duration(intervalSeconds) * time.Second)
		return next, nil
	case "daily":
		hour, minute, err := parseHHMM(dailyTimeUTC)
		if err != nil {
			return time.Time{}, err
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next, nil
	default:
		return time.Time{}, fmt.Errorf("unknown schedule type %q", scheduleType)
	}
}

// AdvancePastNow is like NextRun for 'interval' schedules, except it starts
// from a trigger's last computed next_run_at (which may be long past, if the
// scheduler was stopped) and steps forward until the result is after now,
// instead of always stepping from now — the caller doesn't need this
// distinction for 'daily', where the day-boundary logic in NextRun already
// self-corrects, but 'interval' would otherwise silently shift its phase
// (e.g. a 5-minute trigger drifting off its original :00/:05/:10 alignment)
// every time it fires. Use this from the scheduler loop; use NextRun to seed
// a newly created trigger's initial next_run_at from "now".
func AdvancePastNow(previousRunAt time.Time, now time.Time, scheduleType string, intervalSeconds int, dailyTimeUTC string) (time.Time, error) {
	if scheduleType != "interval" {
		return NextRun(now, scheduleType, intervalSeconds, dailyTimeUTC)
	}
	if intervalSeconds <= 0 {
		return time.Time{}, fmt.Errorf("interval schedule needs a positive interval_seconds")
	}
	next := previousRunAt.UTC()
	step := time.Duration(intervalSeconds) * time.Second
	for !next.After(now.UTC()) {
		next = next.Add(step)
	}
	return next, nil
}

func parseHHMM(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("daily_time_utc must be HH:MM: %w", err)
	}
	return t.Hour(), t.Minute(), nil
}

// FormatTime and ParseTime keep every caller (store layer, scheduler) using
// the same textual timestamp representation, since SQLite stores TEXT.
func FormatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func ParseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

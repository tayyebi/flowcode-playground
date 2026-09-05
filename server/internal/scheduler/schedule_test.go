package scheduler

import (
	"testing"
	"time"
)

func TestNextRunInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	next, err := NextRun(now, "interval", 300, "")
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := now.Add(5 * time.Minute)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestNextRunDailyLaterToday(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	next, err := NextRun(now, "daily", 0, "18:30")
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 1, 1, 18, 30, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestNextRunDailyAlreadyPassedRollsToTomorrow(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	next, err := NextRun(now, "daily", 0, "06:00")
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestNextRunRejectsUnknownScheduleType(t *testing.T) {
	if _, err := NextRun(time.Now(), "cron", 0, ""); err == nil {
		t.Fatal("expected an error for an unsupported schedule type")
	}
}

// TestAdvancePastNowSkipsMissedRunsRatherThanBursting is the behavior that
// matters if the scheduler was stopped for a while: a 1-minute trigger must
// not fire 500 times in a row to catch up, it must skip straight to the next
// occurrence after now (mirroring Apps Script's own behavior when the host
// was unavailable).
func TestAdvancePastNowSkipsMissedRunsRatherThanBursting(t *testing.T) {
	previous := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 8, 17, 30, 0, time.UTC) // ~8.3 hours later
	next, err := AdvancePastNow(previous, now, "interval", 60, "")
	if err != nil {
		t.Fatalf("AdvancePastNow: %v", err)
	}
	if !next.After(now) {
		t.Fatalf("next = %v, want strictly after now = %v", next, now)
	}
	// Should land on the next minute boundary from the original phase, not
	// drift by however long the gap happened to be.
	want := time.Date(2026, 1, 1, 8, 18, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v (phase-aligned, not drifted)", next, want)
	}
}

func TestAdvancePastNowDailyDelegatesToNextRun(t *testing.T) {
	previous := time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	next, err := AdvancePastNow(previous, now, "daily", 0, "09:00")
	if err != nil {
		t.Fatalf("AdvancePastNow: %v", err)
	}
	want := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

func TestFormatAndParseTimeRoundtrip(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s := FormatTime(now)
	got, err := ParseTime(s)
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if !got.Equal(now) {
		t.Errorf("roundtrip = %v, want %v", got, now)
	}
}

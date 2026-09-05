package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/db"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// newTestScheduler wires a Scheduler against a real store and a real Engine
// pointed at nonexistent binaries. That's enough to exercise the
// record-execution-and-reschedule logic without needing the fcc/fcplay
// toolchain: a missing binary still produces a well-formed engine.Result
// (ExitCode -1, Error set), it just never runs anything real.
func newTestScheduler(t *testing.T) (*Scheduler, *store.Store) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	st := store.New(sqlDB)

	eng := engine.New("/nonexistent-fcc", "/nonexistent-fcplay", t.TempDir(),
		time.Second, time.Second, 4096, 2)

	sch := New(st, eng)
	return sch, st
}

func TestFireDueRecordsExecutionAndReschedules(t *testing.T) {
	ctx := context.Background()
	sch, st := newTestScheduler(t)

	p, err := st.CreateProject(ctx, "demo", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	f, err := st.UpsertFile(ctx, p.ID, "main.fc", "workflow: A\n")
	if err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trig, err := st.CreateTrigger(ctx, store.NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: FormatTime(now),
	})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	sch.Now = func() time.Time { return now }
	sch.fireDue(ctx)

	execs, err := st.ListExecutions(ctx, p.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("got %d executions, want 1", len(execs))
	}
	if execs[0].Source != "trigger" {
		t.Errorf("Source = %q, want trigger", execs[0].Source)
	}
	if execs[0].TriggerID == nil || *execs[0].TriggerID != trig.ID {
		t.Errorf("TriggerID = %v, want %d", execs[0].TriggerID, trig.ID)
	}

	updated, err := st.GetTrigger(ctx, trig.ID)
	if err != nil {
		t.Fatalf("GetTrigger: %v", err)
	}
	if updated.LastRunAt != FormatTime(now) {
		t.Errorf("LastRunAt = %q, want %q", updated.LastRunAt, FormatTime(now))
	}
	wantNext := FormatTime(now.Add(60 * time.Second))
	if updated.NextRunAt != wantNext {
		t.Errorf("NextRunAt = %q, want %q", updated.NextRunAt, wantNext)
	}

	// A second tick at the same "now" must not fire again: next_run_at is now
	// in the future relative to the fixed clock.
	sch.fireDue(ctx)
	execs, _ = st.ListExecutions(ctx, p.ID, 10, 0)
	if len(execs) != 1 {
		t.Errorf("got %d executions after a second tick at the same time, want still 1", len(execs))
	}
}

func TestFireDueSkipsDisabledTriggers(t *testing.T) {
	ctx := context.Background()
	sch, st := newTestScheduler(t)

	p, _ := st.CreateProject(ctx, "demo", "")
	f, _ := st.UpsertFile(ctx, p.ID, "main.fc", "workflow: A\n")

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trig, err := st.CreateTrigger(ctx, store.NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: FormatTime(now),
	})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if err := st.SetTriggerEnabled(ctx, trig.ID, false); err != nil {
		t.Fatalf("SetTriggerEnabled: %v", err)
	}

	sch.Now = func() time.Time { return now }
	sch.fireDue(ctx)

	execs, _ := st.ListExecutions(ctx, p.ID, 10, 0)
	if len(execs) != 0 {
		t.Errorf("got %d executions for a disabled trigger, want 0", len(execs))
	}
}

func intPtr(v int) *int { return &v }

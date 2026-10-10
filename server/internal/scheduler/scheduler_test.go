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

// seedWorkspace creates a user, their workspace, a project with one file —
// the minimum the ws_* scheduler path needs.
func seedWorkspace(t *testing.T, st *store.Store) *store.WSProject {
	t.Helper()
	ctx := context.Background()
	u, err := st.UpsertUserBySubject(ctx, "test-subject", "dev@example.com", "Dev", "")
	if err != nil {
		t.Fatalf("UpsertUserBySubject: %v", err)
	}
	ws, err := st.CreateWorkspace(ctx, "Dev")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := st.AddWorkspaceMember(ctx, ws.ID, u.ID, store.RoleOwner); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	p, err := st.CreateWSProject(ctx, ws.ID, u.ID, "demo", "")
	if err != nil {
		t.Fatalf("CreateWSProject: %v", err)
	}
	if _, err := st.UpsertWSFile(ctx, p.ID, "main.fc", "workflow: A\n"); err != nil {
		t.Fatalf("UpsertWSFile: %v", err)
	}
	return p
}

func TestFireDueRecordsExecutionAndReschedules(t *testing.T) {
	ctx := context.Background()
	sch, st := newTestScheduler(t)
	p := seedWorkspace(t, st)
	f, err := st.GetWSFile(ctx, p.ID, "main.fc")
	if err != nil {
		t.Fatalf("GetWSFile: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trig, err := st.CreateWSTrigger(ctx, store.NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: FormatTime(now),
	})
	if err != nil {
		t.Fatalf("CreateWSTrigger: %v", err)
	}

	sch.Now = func() time.Time { return now }
	sch.fireDue(ctx)

	execs, err := st.ListWSExecutions(ctx, p.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListWSExecutions: %v", err)
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
	if execs[0].ActorID == nil {
		t.Errorf("ActorID = nil, want the project owner attributed")
	}

	updated, err := st.GetWSTrigger(ctx, trig.ID)
	if err != nil {
		t.Fatalf("GetWSTrigger: %v", err)
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
	execs, _ = st.ListWSExecutions(ctx, p.ID, 10, 0)
	if len(execs) != 1 {
		t.Errorf("got %d executions after a second tick at the same time, want still 1", len(execs))
	}
}

func TestFireDueSkipsDisabledTriggers(t *testing.T) {
	ctx := context.Background()
	sch, st := newTestScheduler(t)
	p := seedWorkspace(t, st)
	f, _ := st.GetWSFile(ctx, p.ID, "main.fc")

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trig, err := st.CreateWSTrigger(ctx, store.NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: FormatTime(now),
	})
	if err != nil {
		t.Fatalf("CreateWSTrigger: %v", err)
	}
	if err := st.SetWSTriggerEnabled(ctx, trig.ID, false); err != nil {
		t.Fatalf("SetWSTriggerEnabled: %v", err)
	}

	sch.Now = func() time.Time { return now }
	sch.fireDue(ctx)

	execs, _ := st.ListWSExecutions(ctx, p.ID, 10, 0)
	if len(execs) != 0 {
		t.Errorf("got %d executions for a disabled trigger, want 0", len(execs))
	}
}

func intPtr(v int) *int { return &v }

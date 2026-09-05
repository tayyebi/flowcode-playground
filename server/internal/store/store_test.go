package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tayyebi/flowcode-playground/server/internal/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return New(sqlDB)
}

func TestProjectFileVersionRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p, err := s.CreateProject(ctx, "My Project!", "a test project")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.Slug != "my-project" {
		t.Errorf("Slug = %q, want %q", p.Slug, "my-project")
	}

	p2, err := s.CreateProject(ctx, "My Project!", "another one")
	if err != nil {
		t.Fatalf("CreateProject (collision): %v", err)
	}
	if p2.Slug == p.Slug {
		t.Errorf("colliding project got the same slug %q", p2.Slug)
	}

	if _, err := s.UpsertFile(ctx, p.ID, "main.fc", "workflow: A\n"); err != nil {
		t.Fatalf("UpsertFile: %v", err)
	}
	files, err := s.ListFiles(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 || files[0].Content != "workflow: A\n" {
		t.Fatalf("ListFiles = %+v", files)
	}

	if _, err := s.UpsertFile(ctx, p.ID, "main.fc", "workflow: B\n"); err != nil {
		t.Fatalf("UpsertFile (update): %v", err)
	}
	f, err := s.GetFile(ctx, p.ID, "main.fc")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if f.Content != "workflow: B\n" {
		t.Errorf("Content = %q, want the updated content", f.Content)
	}

	v, err := s.CreateVersion(ctx, p.ID, "v1")
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if v.Number != 1 {
		t.Errorf("Number = %d, want 1", v.Number)
	}

	if _, err := s.UpsertFile(ctx, p.ID, "main.fc", "workflow: C\n"); err != nil {
		t.Fatalf("UpsertFile (drift from snapshot): %v", err)
	}
	if err := s.RestoreVersion(ctx, p.ID, v.ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	f, err = s.GetFile(ctx, p.ID, "main.fc")
	if err != nil {
		t.Fatalf("GetFile after restore: %v", err)
	}
	if f.Content != "workflow: B\n" {
		t.Errorf("Content after restore = %q, want the snapshotted content", f.Content)
	}

	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.GetFile(ctx, p.ID, "main.fc"); err != ErrNotFound {
		t.Errorf("file survived project deletion (cascade didn't fire): err=%v", err)
	}
}

func TestDeploymentAndTriggerLookup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p, _ := s.CreateProject(ctx, "demo", "")
	f, _ := s.UpsertFile(ctx, p.ID, "main.fc", "workflow: A\n")

	dep, err := s.CreateDeployment(ctx, p.ID, f.ID, nil)
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got, err := s.GetDeploymentBySlug(ctx, dep.Slug)
	if err != nil {
		t.Fatalf("GetDeploymentBySlug: %v", err)
	}
	if got.FileName != "main.fc" {
		t.Errorf("FileName = %q, want main.fc", got.FileName)
	}

	trig, err := s.CreateTrigger(ctx, NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: "2020-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	due, err := s.DueTriggers(ctx, "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DueTriggers: %v", err)
	}
	if len(due) != 1 || due[0].ID != trig.ID {
		t.Fatalf("DueTriggers = %+v, want [%d]", due, trig.ID)
	}

	if err := s.RecordTriggerRun(ctx, trig.ID, "2030-01-01T00:00:00Z", "2030-01-01T00:01:00Z"); err != nil {
		t.Fatalf("RecordTriggerRun: %v", err)
	}
	notDue, err := s.DueTriggers(ctx, "2030-01-01T00:00:30Z")
	if err != nil {
		t.Fatalf("DueTriggers: %v", err)
	}
	if len(notDue) != 0 {
		t.Errorf("DueTriggers = %+v, want none after recomputing next_run_at", notDue)
	}
}

func TestKVUpsertAndExtract(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p, _ := s.CreateProject(ctx, "demo", "")

	if err := s.UpsertKV(ctx, p.ID, "greeting", "hello", nil); err != nil {
		t.Fatalf("UpsertKV: %v", err)
	}
	if err := s.UpsertKV(ctx, p.ID, "greeting", "hello again", nil); err != nil {
		t.Fatalf("UpsertKV (update): %v", err)
	}
	entries, err := s.ListKV(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListKV: %v", err)
	}
	if len(entries) != 1 || entries[0].Value != "hello again" {
		t.Fatalf("ListKV = %+v", entries)
	}

	writes := ExtractStoreSets(`store set key = "greeting" value = "hello, world"`)
	if writes["greeting"] != "hello, world" {
		t.Errorf("ExtractStoreSets = %+v", writes)
	}
}

func intPtr(v int) *int { return &v }

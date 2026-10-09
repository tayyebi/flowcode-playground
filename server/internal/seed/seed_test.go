package seed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/db"
	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// fakeRunner stands in for the engine: every run "succeeds" and leaves one
// `store set` line in its trace, which is the contract the KV log is parsed
// from. What matters to these tests is the seeding bookkeeping around the
// runs, not the toolchain itself.
type fakeRunner struct{ runs int }

func (f *fakeRunner) Run(ctx context.Context, source string) (*engine.Result, error) {
	f.runs++
	return &engine.Result{
		Compile: engine.CompileResult{StageResult: engine.StageResult{ExitCode: 0}},
		Run: &engine.StageResult{
			ExitCode: 0,
			Stderr:   "[flowcode:INFO] vm completed successfully\n[flowcode:INFO] store set key = \"demo.key\" value = \"v\"\n",
		},
	}, nil
}

const manifest = `{
  "name": "Demo",
  "description": "demo project",
  "files": {
    "main.fc": "files/main-v1.fc",
    "aux.fc": "files/aux.fc"
  },
  "runOnSeed": ["main.fc"],
  "versions": [
    { "label": "v1" },
    { "label": "v2", "files": { "main.fc": "files/main-v2.fc" } }
  ],
  "restoreVersion": 1,
  "deployments": [ { "fileName": "main.fc" } ],
  "triggers": [
    { "fileName": "main.fc", "scheduleType": "interval", "intervalSeconds": 60 }
  ]
}`

// writeExample materializes the fixture manifest above into dir, with v2
// content distinguishable from v1.
func writeExample(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo/project.json", manifest)
	write("demo/files/main-v1.fc", "workflow: V1\n")
	write("demo/files/main-v2.fc", "workflow: V2\n")
	write("demo/files/aux.fc", "workflow: Aux\n")
	// A plain sample directory: no manifest, so the seeder must ignore it.
	write("plain-sample/hello.fc", "workflow: Hello\n")
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return store.New(sqlDB)
}

func quietOpts() Options {
	return Options{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
}

func TestSeedCreatesProjectWithFullStory(t *testing.T) {
	dir := t.TempDir()
	writeExample(t, dir)
	examples, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(examples) != 1 || examples[0].Name != "demo" {
		t.Fatalf("Load returned %+v, want exactly the demo project example", examples)
	}

	st := newStore(t)
	runner := &fakeRunner{}
	if err := Seed(context.Background(), st, runner, examples, quietOpts()); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	p, err := st.GetProjectBySlug(context.Background(), "demo")
	if err != nil {
		t.Fatalf("seeded project not found: %v", err)
	}

	// Restored to v1: the working copy holds v1 content even though v2 was
	// saved and snapshotted after it.
	f, err := st.GetFile(context.Background(), p.ID, "main.fc")
	if err != nil {
		t.Fatalf("main.fc missing: %v", err)
	}
	if f.Content != "workflow: V1\n" {
		t.Errorf("main.fc content after restore = %q, want v1", f.Content)
	}

	versions, err := st.ListVersions(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 || versions[0].Number != 2 || versions[1].Number != 1 {
		t.Errorf("versions = %+v, want v2 and v1", versions)
	}

	deployments, err := st.ListDeployments(context.Background(), p.ID)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployments = %+v, err = %v; want exactly one", deployments, err)
	}

	triggers, err := st.ListTriggers(context.Background(), p.ID)
	if err != nil || len(triggers) != 1 {
		t.Fatalf("triggers = %+v, err = %v; want exactly one", triggers, err)
	}
	if triggers[0].NextRunAt == "" {
		t.Error("trigger has no next_run_at computed")
	}

	// The runOnSeed file ran once and its store set landed in the KV log.
	if runner.runs != 1 {
		t.Errorf("runner called %d times, want 1 (runOnSeed only)", runner.runs)
	}
	executions, err := st.ListExecutions(context.Background(), p.ID, 0, 0)
	if err != nil || len(executions) != 1 {
		t.Fatalf("executions = %+v, err = %v; want the seeded run", executions, err)
	}
	kv, err := st.ListKV(context.Background(), p.ID)
	if err != nil || len(kv) != 1 || kv[0].Key != "demo.key" {
		t.Fatalf("kv = %+v, err = %v; want the trace's store set", kv, err)
	}

	if _, err := st.GetSeededExample(context.Background(), "demo"); err != nil {
		t.Errorf("seeded_examples row missing: %v", err)
	}
}

func TestSeedIsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	writeExample(t, dir)
	examples, _ := Load(dir)
	st := newStore(t)
	runner := &fakeRunner{}

	for i := 0; i < 3; i++ {
		if err := Seed(context.Background(), st, runner, examples, quietOpts()); err != nil {
			t.Fatalf("Seed #%d: %v", i+1, err)
		}
	}

	projects, err := st.ListProjects(context.Background())
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects after 3 seeds = %+v, err = %v; want exactly one", projects, err)
	}
	if runner.runs != 1 {
		t.Errorf("runner called %d times across restarts, want 1", runner.runs)
	}

	// A project deleted through the UI stays deleted: the bookkeeping row,
	// not the project's absence, is what gates re-seeding.
	if err := st.DeleteProject(context.Background(), projects[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := Seed(context.Background(), st, runner, examples, quietOpts()); err != nil {
		t.Fatalf("Seed after delete: %v", err)
	}
	if projects, _ = st.ListProjects(context.Background()); len(projects) != 0 {
		t.Errorf("deleted project resurrected: %+v", projects)
	}
}

func TestSeedAdoptsPreExistingProject(t *testing.T) {
	dir := t.TempDir()
	writeExample(t, dir)
	examples, _ := Load(dir)
	st := newStore(t)

	// A database seeded by hand before seeding was built in: the project
	// already lives under the slug the manifest's name produces.
	existing, err := st.CreateProject(context.Background(), "Demo", "manually seeded")
	if err != nil {
		t.Fatalf("create existing: %v", err)
	}

	runner := &fakeRunner{}
	if err := Seed(context.Background(), st, runner, examples, quietOpts()); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	projects, _ := st.ListProjects(context.Background())
	if len(projects) != 1 || projects[0].ID != existing.ID {
		t.Fatalf("adoption created a duplicate: %+v", projects)
	}
	if runner.runs != 0 {
		t.Errorf("adopted example ran files %d times, want 0", runner.runs)
	}
	if _, err := st.GetSeededExample(context.Background(), "demo"); err != nil {
		t.Errorf("adoption did not record the bookkeeping row: %v", err)
	}
}

func TestSeedRefreshRecreates(t *testing.T) {
	dir := t.TempDir()
	writeExample(t, dir)
	examples, _ := Load(dir)
	st := newStore(t)

	runner := &fakeRunner{}
	if err := Seed(context.Background(), st, runner, examples, quietOpts()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	before, _ := st.ListProjects(context.Background())

	opts := quietOpts()
	opts.Refresh = true
	if err := Seed(context.Background(), st, runner, examples, opts); err != nil {
		t.Fatalf("refresh Seed: %v", err)
	}

	after, _ := st.ListProjects(context.Background())
	if len(after) != 1 {
		t.Fatalf("refresh left %d projects, want 1", len(after))
	}
	if after[0].ID == before[0].ID {
		t.Error("refresh kept the old project id; want a fresh project")
	}
	if runner.runs != 2 {
		t.Errorf("runner called %d times, want 2 (seed + reseed)", runner.runs)
	}
}

func TestLoadRejectsBrokenManifests(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{"no name", `{"files": {"a.fc": "files/a.fc"}}`, "no name"},
		{"no files", `{"name": "X"}`, "no files"},
		{"runOnSeed unknown", `{"name": "X", "files": {"a.fc": "files/a.fc"}, "runOnSeed": ["b.fc"]}`, "not in files"},
		{"escaping path", `{"name": "X", "files": {"a.fc": "../secrets.fc"}}`, "escapes"},
		{"bad restore", `{"name": "X", "files": {"a.fc": "files/a.fc"}, "restoreVersion": 5}`, "outside"},
		{"bad schedule", `{"name": "X", "files": {"a.fc": "files/a.fc"}, "triggers": [{"fileName": "a.fc", "scheduleType": "weekly"}]}`, "unknown scheduleType"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "x", "files"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "x", "project.json"), []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "x", "files", "a.fc"), []byte("workflow: A\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Package seed turns the playground's project examples (directories under
// examples/ with a project.json manifest) into real saved projects — files,
// an initial run of each, version snapshots, a deployment, and triggers —
// using the same store and engine the HTTP handlers use.
//
// Seeding runs in-process at server startup and is exactly-once per database:
// a seeded_examples row (see internal/store/seeded.go) records what has been
// seeded, so restarts skip it and a project deleted through the UI stays
// deleted. No HTTP round-trip is involved, which also means no admin-token
// handshake with the very server doing the seeding.
package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tayyebi/flowcode-playground/server/internal/engine"
	"github.com/tayyebi/flowcode-playground/server/internal/scheduler"
	"github.com/tayyebi/flowcode-playground/server/internal/store"
)

// ManifestFilename marks a directory under examples/ as a project example.
const ManifestFilename = "project.json"

// Manifest is the declarative shape of a project example. It describes the
// seed sequence: initial files, which to run once so the execution history
// and KV log are populated, version snapshots to take (with per-file content
// overrides applied before each snapshot), an optional restore, and the
// deployments and triggers to wire up.
type Manifest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Files       map[string]string `json:"files"` // project file name -> path relative to the example dir
	RunOnSeed   []string          `json:"runOnSeed"`
	Versions    []VersionSpec     `json:"versions"`
	// RestoreVersion is the 1-based version to restore after all snapshots
	// are taken, leaving the project on that version's content. 0 = none.
	RestoreVersion int             `json:"restoreVersion"`
	Deployments    []DeploymentSpec `json:"deployments"`
	Triggers       []TriggerSpec    `json:"triggers"`
}

type VersionSpec struct {
	Label string `json:"label"`
	// Files holds per-file content to apply before taking this snapshot;
	// names must exist in the manifest's Files. The previous version's
	// content is kept for files not mentioned.
	Files map[string]string `json:"files,omitempty"`
}

type DeploymentSpec struct {
	FileName string `json:"fileName"`
}

type TriggerSpec struct {
	FileName        string `json:"fileName"`
	ScheduleType    string `json:"scheduleType"` // 'interval' | 'daily'
	IntervalSeconds *int   `json:"intervalSeconds,omitempty"`
	DailyTimeUTC    string `json:"dailyTimeUtc,omitempty"`
}

// Example is one loaded project example: its manifest plus a hash over every
// file in its directory, so the bookkeeping row can detect content changes.
type Example struct {
	Name     string // directory name, the stable identity in seeded_examples
	Dir      string
	Manifest *Manifest
	Hash     string
}

// Runner is the slice of *engine.Engine the seeder needs. An interface so
// tests can seed against a canned runner instead of the real toolchain.
type Runner interface {
	Run(ctx context.Context, source string) (*engine.Result, error)
}

// Options tunes a Seed run.
type Options struct {
	// Refresh deletes and re-creates examples that were already seeded
	// (the old seeder's --reset).
	Refresh bool
	Now     func() time.Time                       // defaults to time.Now
	Logf    func(format string, args ...interface{}) // defaults to log.Printf
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) logf(format string, args ...interface{}) {
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Load reads every project example under dir (a directory containing a
// project.json). Directories without a manifest are plain samples — the
// picker's business, not ours. Structural problems fail loudly: a broken
// example in the tree is a broken deploy.
func Load(dir string) ([]*Example, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read examples dir: %w", err)
	}

	var examples []*Example
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		exDir := filepath.Join(dir, entry.Name())

		manifestPath := filepath.Join(exDir, ManifestFilename)
		raw, err := os.ReadFile(manifestPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue // plain sample, served by the picker
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", manifestPath, err)
		}

		var m Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("parse %s: %w", manifestPath, err)
		}
		hash, err := hashDir(exDir)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", exDir, err)
		}
		if err := validate(entry.Name(), &m, exDir); err != nil {
			return nil, err
		}

		examples = append(examples, &Example{Name: entry.Name(), Dir: exDir, Manifest: &m, Hash: hash})
	}
	return examples, nil
}

// validate checks a manifest's structural invariants up front so seeding
// fails before half a project exists, not in the middle of creating one.
func validate(name string, m *Manifest, dir string) error {
	if m.Name == "" {
		return fmt.Errorf("example %s: manifest has no name", name)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("example %s: manifest has no files", name)
	}
	for file, rel := range m.Files {
		if err := checkWithin(dir, rel); err != nil {
			return fmt.Errorf("example %s: file %q: %w", name, file, err)
		}
	}
	for _, f := range m.RunOnSeed {
		if _, ok := m.Files[f]; !ok {
			return fmt.Errorf("example %s: runOnSeed names %q which is not in files", name, f)
		}
	}
	for i, v := range m.Versions {
		if v.Label == "" {
			return fmt.Errorf("example %s: version %d has no label", name, i+1)
		}
		for file, rel := range v.Files {
			if _, ok := m.Files[file]; !ok {
				return fmt.Errorf("example %s: version %d changes %q which is not in files", name, i+1, file)
			}
			if err := checkWithin(dir, rel); err != nil {
				return fmt.Errorf("example %s: version %d file %q: %w", name, i+1, file, err)
			}
		}
	}
	if m.RestoreVersion < 0 || m.RestoreVersion > len(m.Versions) {
		return fmt.Errorf("example %s: restoreVersion %d is outside 1..%d", name, m.RestoreVersion, len(m.Versions))
	}
	for i, d := range m.Deployments {
		if _, ok := m.Files[d.FileName]; !ok {
			return fmt.Errorf("example %s: deployment %d names %q which is not in files", name, i+1, d.FileName)
		}
	}
	for i, tr := range m.Triggers {
		if _, ok := m.Files[tr.FileName]; !ok {
			return fmt.Errorf("example %s: trigger %d names %q which is not in files", name, i+1, tr.FileName)
		}
		if tr.ScheduleType != "interval" && tr.ScheduleType != "daily" {
			return fmt.Errorf("example %s: trigger %d has unknown scheduleType %q", name, i+1, tr.ScheduleType)
		}
	}
	return nil
}

// checkWithin proves rel resolves to a readable file inside dir — the
// manifest is data checked into the repo, but it is still parsed input, and
// a stray "../.." path should be rejected rather than trusted.
func checkWithin(dir, rel string) error {
	if rel == "" {
		return errors.New("empty path")
	}
	path := filepath.Clean(filepath.Join(dir, rel))
	inner, err := filepath.Rel(dir, path)
	if err != nil || inner == ".." || strings.HasPrefix(inner, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the example directory", rel)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory", rel)
	}
	return nil
}

// hashDir fingerprints every file under dir (relative paths and contents,
// both sorted) so a seeded_examples row can tell whether an example's
// content has changed since it was seeded.
func hashDir(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Seed seeds every example that this database hasn't recorded as seeded yet.
//
// Per example: skip if already recorded (unless refreshing); adopt a project
// that already lives under the expected slug (a database seeded by hand
// before this machinery existed); otherwise replay the manifest — create the
// project, save the files, run the runnable ones through the engine, take
// the version snapshots, restore, deploy, schedule. One example failing does
// not stop the others; the joined error reports all of them.
func Seed(ctx context.Context, st *store.Store, run Runner, examples []*Example, opts Options) error {
	var errs []error
	for _, ex := range examples {
		if err := seedOne(ctx, st, run, ex, opts); err != nil {
			opts.logf("seed: %v", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func seedOne(ctx context.Context, st *store.Store, run Runner, ex *Example, opts Options) error {
	m := ex.Manifest

	if row, err := st.GetSeededExample(ctx, ex.Name); err == nil {
		if !opts.Refresh {
			opts.logf("seed: %s already seeded (project %q, %s) — skipping", ex.Name, row.Slug, row.SeededAt)
			return nil
		}
		if p, err := st.GetProject(ctx, row.ProjectID); err == nil {
			if err := st.DeleteProject(ctx, p.ID); err != nil {
				return fmt.Errorf("refresh %s: delete old project: %w", ex.Name, err)
			}
		}
		if err := st.DeleteSeededExample(ctx, ex.Name); err != nil {
			return fmt.Errorf("refresh %s: %w", ex.Name, err)
		}
		opts.logf("seed: refresh: deleted previous %s project", ex.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("look up seeded %s: %w", ex.Name, err)
	}

	// Adoption: the database already holds a project under the slug this
	// manifest's name will produce (seeded by hand before seeding was built
	// in). Record it rather than creating a suffixed duplicate next to it.
	expected := store.Slugify(m.Name)
	if p, err := st.GetProjectBySlug(ctx, expected); err == nil {
		if err := st.RecordSeededExample(ctx, ex.Name, p.Slug, p.ID, ex.Hash); err != nil {
			return err
		}
		opts.logf("seed: %s: adopted existing project %q (id %d) — skipping", ex.Name, p.Slug, p.ID)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("look up project by slug %q: %w", expected, err)
	}

	p, err := st.CreateProject(ctx, m.Name, m.Description)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}

	// Sorted names so file positions (and thus the UI order) are the same on
	// every seed, not whatever order map iteration picks.
	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)

	opts.logf("seed: %s: creating project %q (id %d)", ex.Name, p.Slug, p.ID)
	for _, name := range names {
		content, err := readExampleFile(ex.Dir, m.Files[name])
		if err != nil {
			return err
		}
		if _, err := st.UpsertFile(ctx, p.ID, name, content); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
	}

	for _, name := range m.RunOnSeed {
		if err := runAndRecord(ctx, st, run, p.ID, name, opts); err != nil {
			return err
		}
	}

	for _, v := range m.Versions {
		for _, name := range sortedKeys(v.Files) {
			content, err := readExampleFile(ex.Dir, v.Files[name])
			if err != nil {
				return err
			}
			if _, err := st.UpsertFile(ctx, p.ID, name, content); err != nil {
				return fmt.Errorf("update %s for version %q: %w", name, v.Label, err)
			}
		}
		if _, err := st.CreateVersion(ctx, p.ID, v.Label); err != nil {
			return fmt.Errorf("save version %q: %w", v.Label, err)
		}
	}
	if m.RestoreVersion > 0 {
		v, err := st.GetVersionByNumber(ctx, p.ID, m.RestoreVersion)
		if err != nil {
			return fmt.Errorf("look up version %d to restore: %w", m.RestoreVersion, err)
		}
		if err := st.RestoreVersion(ctx, p.ID, v.ID); err != nil {
			return fmt.Errorf("restore version %d: %w", m.RestoreVersion, err)
		}
	}

	var deployURLs []string
	for _, d := range m.Deployments {
		f, err := st.GetFile(ctx, p.ID, d.FileName)
		if err != nil {
			return fmt.Errorf("look up %s to deploy: %w", d.FileName, err)
		}
		dep, err := st.CreateDeployment(ctx, p.ID, f.ID, nil)
		if err != nil {
			return fmt.Errorf("deploy %s: %w", d.FileName, err)
		}
		deployURLs = append(deployURLs, "/deploy/"+dep.Slug)
	}

	var triggerDescs []string
	for _, tr := range m.Triggers {
		f, err := st.GetFile(ctx, p.ID, tr.FileName)
		if err != nil {
			return fmt.Errorf("look up %s to schedule: %w", tr.FileName, err)
		}
		interval := 0
		if tr.IntervalSeconds != nil {
			interval = *tr.IntervalSeconds
		}
		next, err := scheduler.NextRun(opts.now(), tr.ScheduleType, interval, tr.DailyTimeUTC)
		if err != nil {
			return fmt.Errorf("schedule %s: %w", tr.FileName, err)
		}
		if _, err := st.CreateTrigger(ctx, store.NewTrigger{
			ProjectID: p.ID, FileID: f.ID,
			ScheduleType: tr.ScheduleType, IntervalSeconds: tr.IntervalSeconds, DailyTimeUTC: tr.DailyTimeUTC,
			NextRunAt: scheduler.FormatTime(next),
		}); err != nil {
			return fmt.Errorf("create trigger for %s: %w", tr.FileName, err)
		}
		switch tr.ScheduleType {
		case "interval":
			triggerDescs = append(triggerDescs, fmt.Sprintf("%s every %ds", tr.FileName, interval))
		case "daily":
			triggerDescs = append(triggerDescs, fmt.Sprintf("%s daily at %s UTC", tr.FileName, tr.DailyTimeUTC))
		}
	}

	if err := st.RecordSeededExample(ctx, ex.Name, p.Slug, p.ID, ex.Hash); err != nil {
		return err
	}

	opts.logf("seed: %s ready — dashboard /#/dashboard, project /#/projects/%d", ex.Name, p.ID)
	for _, u := range deployURLs {
		opts.logf("seed: on-demand run %s", u)
	}
	if len(triggerDescs) > 0 {
		opts.logf("seed: triggers: %s", strings.Join(triggerDescs, "; "))
	}
	return nil
}

// runAndRecord compiles and runs one file through the shared engine and
// records it exactly like a manual project run would be, so the execution
// history and KV log are populated from the very first minute. A file that
// does not compile or run cleanly is a broken example and fails the seed.
func runAndRecord(ctx context.Context, st *store.Store, run Runner, projectID int64, name string, opts Options) error {
	f, err := st.GetFile(ctx, projectID, name)
	if err != nil {
		return fmt.Errorf("look up %s to run: %w", name, err)
	}

	started := opts.now()
	result, runErr := run.Run(ctx, f.Content)
	finished := opts.now()

	if runErr != nil {
		return fmt.Errorf("run %s: %w", name, runErr)
	}
	if result.Compile.ExitCode != 0 || result.Run == nil || result.Run.ExitCode != 0 {
		return fmt.Errorf("run %s: example does not compile+run cleanly: %+v", name, result)
	}
	if _, err := st.RecordRunWithKV(ctx, store.NewExecution{
		ProjectID: projectID, FileID: &f.ID, FileName: f.Name, Source: "project-run",
		StartedAt: started.UTC().Format(time.RFC3339), FinishedAt: finished.UTC().Format(time.RFC3339),
		Result: result,
	}); err != nil {
		return fmt.Errorf("record run of %s: %w", name, err)
	}
	opts.logf("seed: ran %s: completed, trace recorded", name)
	return nil
}

func readExampleFile(dir, rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	return string(data), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

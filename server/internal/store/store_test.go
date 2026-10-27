package store

import (
	"context"
	"strings"
	"testing"

	"github.com/tayyebi/flowcode-playground/server/internal/testdb"
)

// newTestStore opens a Store in a throwaway schema (see internal/testdb);
// without TEST_DATABASE_URL the tests skip.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(testdb.Open(t))
}

func TestMigrationSeedsSettings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, key := range []string{"mail", "http", "logger", "quotas", "run"} {
		v, err := s.GetSetting(ctx, key)
		if err != nil {
			t.Fatalf("GetSetting(%s): %v", key, err)
		}
		if v == "" {
			t.Errorf("settings group %q was not seeded by the migration", key)
		}
	}
}

func TestUserWorkspaceProjectRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.UpsertUserBySubject(ctx, "sub-1", "alice@example.com", "Alice", "")
	if err != nil {
		t.Fatalf("UpsertUserBySubject: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("Email = %q", u.Email)
	}

	ws, err := s.CreateWorkspace(ctx, "Alice's Space")
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws.Slug != "alice-s-space" {
		t.Errorf("Slug = %q, want %q", ws.Slug, "alice-s-space")
	}
	if err := s.AddWorkspaceMember(ctx, ws.ID, u.ID, RoleOwner); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	list, err := s.ListWorkspacesForUser(ctx, u.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspacesForUser = %+v, %v", list, err)
	}

	p1, err := s.CreateWSProject(ctx, ws.ID, u.ID, "My Project!", "a test project")
	if err != nil {
		t.Fatalf("CreateWSProject: %v", err)
	}
	if p1.Slug != "my-project" {
		t.Errorf("Slug = %q, want %q", p1.Slug, "my-project")
	}
	p2, err := s.CreateWSProject(ctx, ws.ID, u.ID, "My Project!", "another one")
	if err != nil {
		t.Fatalf("CreateWSProject (collision): %v", err)
	}
	if p2.Slug == p1.Slug {
		t.Errorf("colliding project got the same slug %q", p1.Slug)
	}

	role, err := s.EffectiveProjectRole(ctx, p1.ID, u.ID)
	if err != nil || role != RoleOwner {
		t.Fatalf("EffectiveProjectRole = %q, %v; want owner", role, err)
	}
	bob, err := s.UpsertUserBySubject(ctx, "sub-2", "bob@example.com", "Bob", "")
	if err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	role, err = s.EffectiveProjectRole(ctx, p1.ID, bob.ID)
	if err != nil || role != "" {
		t.Fatalf("outsider EffectiveProjectRole = %q, %v; want empty", role, err)
	}

	if err := s.DeleteWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if _, err := s.GetWSFile(ctx, p1.ID, "main.fc"); err != ErrNotFound {
		t.Errorf("project survived workspace deletion (cascade didn't fire): err=%v", err)
	}
}

func TestWSFileVersionsRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, _ := s.UpsertUserBySubject(ctx, "sub-1", "alice@example.com", "Alice", "")
	ws, _ := s.CreateWorkspace(ctx, "w")
	_ = s.AddWorkspaceMember(ctx, ws.ID, u.ID, RoleOwner)
	p, _ := s.CreateWSProject(ctx, ws.ID, u.ID, "demo", "")

	if _, err := s.UpsertWSFile(ctx, p.ID, "main.fc", "workflow: A\n"); err != nil {
		t.Fatalf("UpsertWSFile: %v", err)
	}
	if _, err := s.UpsertWSFile(ctx, p.ID, "main.fc", "workflow: B\n"); err != nil {
		t.Fatalf("UpsertWSFile (update): %v", err)
	}
	f, err := s.GetWSFile(ctx, p.ID, "main.fc")
	if err != nil {
		t.Fatalf("GetWSFile: %v", err)
	}
	if f.Content != "workflow: B\n" {
		t.Errorf("Content = %q, want the updated content", f.Content)
	}

	v, err := s.CreateWSVersion(ctx, p.ID, "v1")
	if err != nil {
		t.Fatalf("CreateWSVersion: %v", err)
	}
	if v.Number != 1 {
		t.Errorf("Number = %d, want 1", v.Number)
	}

	if _, err := s.UpsertWSFile(ctx, p.ID, "main.fc", "workflow: C\n"); err != nil {
		t.Fatalf("UpsertWSFile (drift from snapshot): %v", err)
	}
	if err := s.RestoreWSVersion(ctx, p.ID, v.ID); err != nil {
		t.Fatalf("RestoreWSVersion: %v", err)
	}
	f, err = s.GetWSFile(ctx, p.ID, "main.fc")
	if err != nil {
		t.Fatalf("GetWSFile after restore: %v", err)
	}
	if f.Content != "workflow: B\n" {
		t.Errorf("Content after restore = %q, want the snapshotted content", f.Content)
	}
}

func TestWSDeploymentAndTriggerLookup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, _ := s.UpsertUserBySubject(ctx, "sub-1", "alice@example.com", "Alice", "")
	ws, _ := s.CreateWorkspace(ctx, "w")
	_ = s.AddWorkspaceMember(ctx, ws.ID, u.ID, RoleOwner)
	p, _ := s.CreateWSProject(ctx, ws.ID, u.ID, "demo", "")
	f, _ := s.UpsertWSFile(ctx, p.ID, "main.fc", "workflow: A\n")

	dep, err := s.CreateWSDeployment(ctx, p.ID, f.ID, nil)
	if err != nil {
		t.Fatalf("CreateWSDeployment: %v", err)
	}
	got, err := s.GetWSDeploymentBySlug(ctx, dep.Slug)
	if err != nil {
		t.Fatalf("GetWSDeploymentBySlug: %v", err)
	}
	if got.FileName != "main.fc" || !got.Enabled {
		t.Errorf("got = %+v, want enabled main.fc", got)
	}
	if err := s.SetWSDeploymentEnabled(ctx, dep.ID, false); err != nil {
		t.Fatalf("SetWSDeploymentEnabled: %v", err)
	}
	if got, _ := s.GetWSDeployment(ctx, dep.ID); got.Enabled {
		t.Errorf("deployment still enabled after disable")
	}

	trig, err := s.CreateWSTrigger(ctx, NewTrigger{
		ProjectID: p.ID, FileID: f.ID, ScheduleType: "interval",
		IntervalSeconds: intPtr(60), NextRunAt: "2020-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("CreateWSTrigger: %v", err)
	}

	due, err := s.WSDueTriggers(ctx, "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("WSDueTriggers: %v", err)
	}
	if len(due) != 1 || due[0].ID != trig.ID {
		t.Fatalf("WSDueTriggers = %+v, want [%d]", due, trig.ID)
	}

	if err := s.RecordWSTriggerRun(ctx, trig.ID, "2030-01-01T00:00:00Z", "2030-01-01T00:01:00Z"); err != nil {
		t.Fatalf("RecordWSTriggerRun: %v", err)
	}
	notDue, err := s.WSDueTriggers(ctx, "2030-01-01T00:00:30Z")
	if err != nil {
		t.Fatalf("WSDueTriggers: %v", err)
	}
	if len(notDue) != 0 {
		t.Errorf("WSDueTriggers = %+v, want none after recomputing next_run_at", notDue)
	}
}

func TestWSKVUpsertAndExtract(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, _ := s.UpsertUserBySubject(ctx, "sub-1", "alice@example.com", "Alice", "")
	ws, _ := s.CreateWorkspace(ctx, "w")
	_ = s.AddWorkspaceMember(ctx, ws.ID, u.ID, RoleOwner)
	p, _ := s.CreateWSProject(ctx, ws.ID, u.ID, "demo", "")

	if err := s.UpsertWSKV(ctx, p.ID, "greeting", "hello", nil); err != nil {
		t.Fatalf("UpsertWSKV: %v", err)
	}
	if err := s.UpsertWSKV(ctx, p.ID, "greeting", "hello again", nil); err != nil {
		t.Fatalf("UpsertWSKV (update): %v", err)
	}
	entries, err := s.ListWSKV(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListWSKV: %v", err)
	}
	if len(entries) != 1 || entries[0].Value != "hello again" {
		t.Fatalf("ListWSKV = %+v", entries)
	}

	// A realistic fcplay trace: the store-set dump lines land after
	// "vm completed successfully", one per key, in the exact format
	// runner/fcplay.c emits (which the entrypoint's build check pins).
	trace := "[flowcode:DEBUG] registered 17 builtin plugins\n" +
		"[flowcode:INFO] vm starting, 3 instructions\n" +
		"[flowcode:DEBUG] builtin plugin http.get invoked (no-op pass-through)\n" +
		"[flowcode:INFO] vm completed successfully\n" +
		"[flowcode:INFO] store set key = \"greeting\" value = \"hello, world\"\n" +
		"[flowcode:INFO] store set key = \"onboarding.42\" value = \"{}\"\n"
	writes := ExtractStoreSets(trace)
	if len(writes) != 2 || writes["greeting"] != "hello, world" || writes["onboarding.42"] != "{}" {
		t.Errorf("ExtractStoreSets = %+v", writes)
	}
}

func TestAppQuotas(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, _ := s.UpsertUserBySubject(ctx, "sub-1", "alice@example.com", "Alice", "")

	limit, enabled, err := s.EffectiveQuota(ctx, u.ID, "mail", 10)
	if err != nil || limit != 10 || !enabled {
		t.Fatalf("EffectiveQuota (no override) = %d, %v, %v", limit, enabled, err)
	}

	n := 3
	if err := s.UpsertAppQuota(ctx, AppQuota{UserID: u.ID, App: "mail", CallsPerMinute: &n, Enabled: true}); err != nil {
		t.Fatalf("UpsertAppQuota: %v", err)
	}
	limit, enabled, err = s.EffectiveQuota(ctx, u.ID, "mail", 10)
	if err != nil || limit != 3 || !enabled {
		t.Fatalf("EffectiveQuota (override) = %d, %v, %v", limit, enabled, err)
	}

	if err := s.UpsertAppQuota(ctx, AppQuota{UserID: u.ID, App: "mail", Enabled: false}); err != nil {
		t.Fatalf("UpsertAppQuota (disable): %v", err)
	}
	_, enabled, err = s.EffectiveQuota(ctx, u.ID, "mail", 10)
	if err != nil || enabled {
		t.Fatalf("EffectiveQuota (disabled) = enabled=%v, err=%v", enabled, err)
	}

	if err := s.DeleteAppQuota(ctx, u.ID, "mail"); err != nil {
		t.Fatalf("DeleteAppQuota: %v", err)
	}
	quotas, err := s.ListAppQuotas(ctx, "mail")
	if err != nil || len(quotas) != 0 {
		t.Fatalf("ListAppQuotas after delete = %+v, %v", quotas, err)
	}
}

func TestAdministrators(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Bootstrap seeding: only fires on an empty table.
	if n, err := s.SeedAdministrators(ctx, []string{"Root@example.com", "ops@example.com"}); err != nil || n != 2 {
		t.Fatalf("SeedAdministrators = %d, %v; want 2, nil", n, err)
	}
	if n, _ := s.SeedAdministrators(ctx, []string{"late@example.com"}); n != 0 {
		t.Fatalf("SeedAdministrators on non-empty table inserted %d rows", n)
	}
	if _, err := s.AddAdministrator(ctx, "root@example.com", ""); err == nil {
		t.Errorf("duplicate email (different case) was accepted")
	}

	// Login matching: by subject once bound, by unbound email before that.
	a, err := s.FindAdministratorForLogin(ctx, "admin-sub-1", "root@example.com")
	if err != nil {
		t.Fatalf("FindAdministratorForLogin (unbound email): %v", err)
	}
	if !strings.EqualFold(a.Email, "root@example.com") {
		t.Fatalf("matched wrong row: %+v", a)
	}
	if err := s.UpsertAdministratorLogin(ctx, a.ID, "admin-sub-1", "Root Admin", ""); err != nil {
		t.Fatalf("UpsertAdministratorLogin: %v", err)
	}
	if a, err = s.FindAdministratorForLogin(ctx, "admin-sub-1", "whatever@example.com"); err != nil || a.Subject != "admin-sub-1" {
		t.Fatalf("FindAdministratorForLogin (bound subject) = %+v, %v", a, err)
	}
	// A row whose email matches but whose subject is bound to somebody else
	// must not match a different subject.
	if _, err := s.FindAdministratorForLogin(ctx, "admin-sub-2", "root@example.com"); err != ErrNotFound {
		t.Fatalf("foreign subject claimed a bound email: err=%v", err)
	}
	if _, err := s.FindAdministratorForLogin(ctx, "admin-sub-3", "stranger@example.com"); err != ErrNotFound {
		t.Fatalf("stranger matched the allowlist: err=%v", err)
	}

	if err := s.RemoveAdministrator(ctx, a.ID); err != nil {
		t.Fatalf("RemoveAdministrator: %v", err)
	}
	if n, _ := s.CountAdministrators(ctx); n != 1 {
		t.Fatalf("CountAdministrators = %d, want 1", n)
	}
}

func intPtr(v int) *int { return &v }

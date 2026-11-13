package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// openTemp returns a fresh SQLite store, or a Postgres schema of its own when
// SANDBOXD_TEST_POSTGRES_URL is set.
func openTemp(t *testing.T) *Store { return OpenForTest(t) }

func TestOpenCreatesSchema(t *testing.T) {
	s := openTemp(t)
	for _, table := range []string{"api_keys", "sandboxes", "usage_records", "settings"} {
		if _, err := s.db.Exec("SELECT count(*) FROM " + table); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

func TestAPIKeys(t *testing.T) {
	s := openTemp(t)
	rec, plaintext, err := s.CreateKey(t.Context(), "prod", "default")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plaintext == "" || rec.ID == "" {
		t.Fatalf("empty key/id: %q %q", plaintext, rec.ID)
	}
	got, err := s.ValidateKey(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.OrgID != "default" || got.Name != "prod" {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.ValidateKey(t.Context(), "sf_live_wrong"); err == nil {
		t.Fatalf("expected error for wrong key")
	}
	if err := s.RevokeKey(t.Context(), rec.ID, "default", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.ValidateKey(t.Context(), plaintext); err == nil {
		t.Fatalf("expected revoked key to fail validation")
	}
}

func TestSandboxRecords(t *testing.T) {
	s := openTemp(t)
	rec := SandboxRecord{
		ID: "sb1", Template: "python-3.11", OrgID: "default", State: "running",
		CreatedAt: 100, ExpiresAt: 200, UpdatedAt: 100,
	}
	if err := s.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec.State = "paused"
	rec.UpdatedAt = 150
	if err := s.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("resave: %v", err)
	}
	list, err := s.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].State != "paused" {
		t.Fatalf("got %+v", list)
	}
	if err := s.DeleteSandbox(t.Context(), "sb1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ = s.LoadSandboxes(t.Context())
	if len(list) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(list))
	}
}

func TestSandboxRecordAppConfigRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := SandboxRecord{
		ID: "sb1", Template: "python-3.11", OrgID: "default", State: "paused",
		Snapshot: "/snap/sb1", StartCmd: "python3 app.py", PublicPort: 8000,
		CreatedAt: 100, ExpiresAt: 200, UpdatedAt: 100,
	}
	if err := s.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	list, err := s.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].StartCmd != "python3 app.py" || list[0].PublicPort != 8000 {
		t.Fatalf("got %+v", list)
	}
}

// -1 means always-on and must survive a restart.
func TestSandboxRecordIdleTimeoutRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := SandboxRecord{
		ID: "sb-idle", Template: "node", OrgID: "default", State: "running",
		IdleTimeoutSecs: -1, CreatedAt: 100, ExpiresAt: 200, UpdatedAt: 100,
	}
	if err := s.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	list, err := s.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].IdleTimeoutSecs != -1 {
		t.Fatalf("idle_timeout did not round-trip: %+v", list)
	}
}

func TestSandboxRecordEnvRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := SandboxRecord{
		ID: "sb1", Template: "python-3.11", OrgID: "default", State: "paused",
		StartCmd: "python3 app.py", PublicPort: 8000, Env: `{"TAG":"prod"}`,
		CreatedAt: 100, ExpiresAt: 200, UpdatedAt: 100,
	}
	if err := s.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	list, _ := s.LoadSandboxes(t.Context())
	if len(list) != 1 || list[0].Env != `{"TAG":"prod"}` {
		t.Fatalf("env not persisted: %+v", list)
	}
}

// A pre-migration-framework database (old sandboxes table, no schema_migrations)
// must be re-baselined in place on reopen.
func TestOpenMigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS schema_migrations`,
		`DROP TABLE sandboxes`,
		`CREATE TABLE sandboxes (
			id TEXT PRIMARY KEY, template TEXT NOT NULL,
			org_id TEXT NOT NULL DEFAULT 'default', state TEXT NOT NULL,
			snapshot TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO sandboxes VALUES ('sb_old', 'python-3.11', 'default', 'paused', '/snap/sb_old', 100, 200, 100)`,
	} {
		if _, err := old.db.Exec(stmt); err != nil {
			t.Fatalf("prepare old schema: %v", err)
		}
	}
	old.Close()

	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen (migration): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	list, err := s.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("load after migration: %v", err)
	}
	if len(list) != 1 || list[0].ID != "sb_old" || list[0].StartCmd != "" || list[0].PublicPort != 0 {
		t.Fatalf("got %+v", list)
	}
}

func TestListKeys(t *testing.T) {
	s := openTemp(t)
	a, _, _ := s.CreateKey(t.Context(), "prod", "default")
	_, _, _ = s.CreateKey(t.Context(), "ci", "default")
	list, err := s.ListKeys(t.Context(), "default")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("keys = %d, want 2", len(list))
	}
	if err := s.RevokeKey(t.Context(), a.ID, "default", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, _ = s.ListKeys(t.Context(), "default")
	if len(list) != 1 || list[0].Name != "ci" {
		t.Fatalf("after revoke got %+v", list)
	}
	if strings.Contains(list[0].Masked, "sf_live_") && len(list[0].Masked) > 16 {
		t.Fatalf("masked key looks like a full secret: %q", list[0].Masked)
	}
}

// A foreign org's revoke must report ErrKeyNotFound, same as an unknown id.
func TestRevokeKeyIsOrgScoped(t *testing.T) {
	s := openTemp(t)
	victim, plaintext, err := s.CreateKey(t.Context(), "prod", "u:victim")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.RevokeKey(t.Context(), victim.ID, "u:attacker", true); err != ErrKeyNotFound {
		t.Fatalf("cross-org revoke err = %v, want ErrKeyNotFound", err)
	}
	if _, err := s.ValidateKey(t.Context(), plaintext); err != nil {
		t.Fatalf("key died at another org's hands: %v", err)
	}
	if err := s.RevokeKey(t.Context(), "key_does_not_exist", "u:attacker", true); err != ErrKeyNotFound {
		t.Fatalf("unknown-id revoke err = %v, want ErrKeyNotFound", err)
	}

	if err := s.RevokeKey(t.Context(), victim.ID, "u:victim", true); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if _, err := s.ValidateKey(t.Context(), plaintext); err == nil {
		t.Fatal("owner's revoke did not take effect")
	}
}

func TestUsage(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordUsage(t.Context(), "sb1", "default", UsageDelta{VCPUSeconds: 2.0, RAMGBSeconds: 1.0}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordUsage(t.Context(), "sb1", "default", UsageDelta{VCPUSeconds: 3.0, RAMGBSeconds: 1.5}); err != nil {
		t.Fatalf("record: %v", err)
	}
	sum, err := s.UsageSummary(t.Context(), "default")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.VCPUSeconds != 5.0 || sum.RAMGBSeconds != 2.5 {
		t.Fatalf("got %+v, want vcpu=5 ram=2.5", sum)
	}
}

func TestFunctionEnvAndColdStartsRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := FunctionRecord{
		Name: "f", OrgID: "default", Template: "python-3.11",
		StartCmd: "python3 handler.py", Port: 8000,
		Env: `{"API_KEY":"secret","LOG":"debug"}`, ColdStarts: 2,
		CreatedAt: 100, UpdatedAt: 100,
	}
	if err := s.SaveFunction(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := s.GetFunction(t.Context(), "default", "f")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if got.Env != `{"API_KEY":"secret","LOG":"debug"}` || got.ColdStarts != 2 {
		t.Fatalf("got env=%q cold=%d", got.Env, got.ColdStarts)
	}
}

func TestOpenMigratesFunctionsOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS schema_migrations`,
		`DROP TABLE functions`,
		`CREATE TABLE functions (
			name TEXT NOT NULL, org_id TEXT NOT NULL DEFAULT 'default', template TEXT NOT NULL,
			start_cmd TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 8000,
			sandbox_id TEXT NOT NULL DEFAULT '', invokes INTEGER NOT NULL DEFAULT 0,
			last_invoked_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, PRIMARY KEY (org_id, name))`,
		`INSERT INTO functions (name, org_id, template, created_at, updated_at)
		   VALUES ('old', 'default', 'python-3.11', 100, 100)`,
	} {
		if _, err := old.db.Exec(stmt); err != nil {
			t.Fatalf("prepare old schema: %v", err)
		}
	}
	old.Close()

	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen (migration): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	got, ok, err := s.GetFunction(t.Context(), "default", "old")
	if err != nil || !ok {
		t.Fatalf("get after migration: %v ok=%v", err, ok)
	}
	if got.Env != "{}" || got.ColdStarts != 0 {
		t.Fatalf("migrated defaults wrong: env=%q cold=%d", got.Env, got.ColdStarts)
	}
}

func TestFunctionRecordRoundTrip(t *testing.T) {
	s := openTemp(t)
	rec := FunctionRecord{
		Name: "hello", OrgID: "default", Template: "python-3.11",
		StartCmd: "python3 handler.py", Port: 8000, SandboxID: "sb_1",
		Invokes: 3, LastInvokedAt: 1700, CreatedAt: 100, UpdatedAt: 150,
	}
	if err := s.SaveFunction(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec.Invokes = 4
	rec.SandboxID = ""
	if err := s.SaveFunction(t.Context(), rec); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, ok, err := s.GetFunction(t.Context(), "default", "hello")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if got.Invokes != 4 || got.SandboxID != "" || got.Port != 8000 {
		t.Fatalf("got %+v", got)
	}
	other := rec
	other.OrgID = "acme"
	if err := s.SaveFunction(t.Context(), other); err != nil {
		t.Fatalf("save other org: %v", err)
	}
	list, err := s.LoadFunctions(t.Context(), "default")
	if err != nil || len(list) != 1 {
		t.Fatalf("load default: %v len=%d", err, len(list))
	}
	if err := s.DeleteFunction(t.Context(), "default", "hello"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := s.GetFunction(t.Context(), "default", "hello"); ok {
		t.Fatalf("still present after delete")
	}
}

func TestSandboxPauseArchiveRoundTrips(t *testing.T) {
	st := openTestStore(t)
	rec := SandboxRecord{ID: "sb-pa", OrgID: "o", Template: "base", State: "paused", PauseArchive: "p1",
		CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9}
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.SandboxByID(t.Context(), "sb-pa")
	if err != nil || !ok || got.PauseArchive != "p1" {
		t.Fatalf("pause_archive = %q (%v %v), want p1", got.PauseArchive, ok, err)
	}
	rec.State, rec.PauseArchive = "running", ""
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.SandboxByID(t.Context(), "sb-pa"); got.PauseArchive != "" {
		t.Fatalf("a resumed row still names archive %q", got.PauseArchive)
	}
}

func TestSandboxImageAndPortsRoundTrip(t *testing.T) {
	st := openTestStore(t)
	rec := SandboxRecord{ID: "sb-img", OrgID: "o", Template: "base", State: "running",
		Image: "example/app:1", Ports: "[8080]", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9}
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.SandboxByID(t.Context(), "sb-img")
	if err != nil || !ok || got.Image != "example/app:1" || got.Ports != "[8080]" {
		t.Fatalf("image, ports = %q, %q (%v %v), want example/app:1, [8080]", got.Image, got.Ports, ok, err)
	}
}

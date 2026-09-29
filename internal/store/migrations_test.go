package store

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestPendingMigrations(t *testing.T) {
	all := []migration{{version: 1}, {version: 2}, {version: 3}}

	if got := pendingMigrations(map[int]bool{}, all); len(got) != 3 {
		t.Fatalf("nothing applied: want 3 pending, got %d", len(got))
	}
	got := pendingMigrations(map[int]bool{1: true}, all)
	if len(got) != 2 || got[0].version != 2 || got[1].version != 3 {
		t.Fatalf("v1 applied: want [2,3], got %+v", got)
	}
	if got := pendingMigrations(map[int]bool{1: true, 2: true, 3: true}, all); len(got) != 0 {
		t.Fatalf("all applied: want 0 pending, got %d", len(got))
	}
}

func TestMigrationsRecordVersion(t *testing.T) {
	st := openTemp(t)
	var version int
	var name string
	if err := st.queryRow(t.Context(),
		`SELECT version, name FROM schema_migrations WHERE version = 1`,
	).Scan(&version, &name); err != nil {
		t.Fatalf("migration v1 not recorded: %v", err)
	}
	if version != 1 || name != "initial_schema" {
		t.Fatalf("recorded version=%d name=%q, want 1/initial_schema", version, name)
	}
}

func TestReopenDoesNotReapplyMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s1.Close()
	s2, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var count int
	if err := s2.queryRow(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("schema_migrations rows = %d after two Opens, want %d (run once)", count, len(migrations))
	}
}

func TestMigrationV9AuditDetail(t *testing.T) {
	st := openTemp(t)
	if _, err := st.exec(t.Context(),
		`INSERT INTO audit_log (org_id, key_id, action, target, at, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		"o", "k", "sandbox.exec", "sb-1", int64(1), `{"cmd":"ls"}`,
	); err != nil {
		t.Fatalf("insert with detail: %v", err)
	}
	var detail string
	if err := st.queryRow(t.Context(), `SELECT detail FROM audit_log WHERE target = 'sb-1'`).Scan(&detail); err != nil || detail != `{"cmd":"ls"}` {
		t.Fatalf("detail = %q err=%v", detail, err)
	}
	var name string
	if err := st.queryRow(t.Context(), `SELECT name FROM schema_migrations WHERE version = 9`).Scan(&name); err != nil || name != "audit_detail" {
		t.Fatalf("v9 not recorded (name=%q err=%v)", name, err)
	}
}

func TestMigrationV10SandboxClient(t *testing.T) {
	st := openTemp(t)
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-v10", Template: "node", OrgID: "default", State: "running",
		CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := st.exec(t.Context(), `UPDATE sandboxes SET client = ? WHERE id = ?`, "cli", "sb-v10"); err != nil {
		t.Fatalf("sandboxes.client column missing: %v", err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{ID: "sh-v10", OrgID: "default", DestroyedAt: 3}); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}
	if _, err := st.exec(t.Context(), `UPDATE sandbox_history SET client = ? WHERE id = ?`, "mcp", "sh-v10"); err != nil {
		t.Fatalf("sandbox_history.client column missing: %v", err)
	}
	var name string
	if err := st.queryRow(t.Context(), `SELECT name FROM schema_migrations WHERE version = 10`).Scan(&name); err != nil || name != "sandbox_client" {
		t.Fatalf("v10 not recorded (name=%q err=%v)", name, err)
	}
}

// A forgotten version must re-apply cleanly against an already-current schema.
func TestMigrationV2HealsUpgradedDB(t *testing.T) {
	st := openTemp(t)

	if _, err := st.exec(t.Context(), `DROP TABLE sandbox_history`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := st.exec(t.Context(), `DELETE FROM schema_migrations WHERE version >= 2`); err != nil {
		t.Fatalf("forget history migrations: %v", err)
	}

	if err := st.migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{ID: "sb-heal", OrgID: "default", DestroyedAt: 1}); err != nil {
		t.Fatalf("RecordHistory after heal (table not recreated?): %v", err)
	}
}

// A reused version is silently skipped on any database that already passed it.
func TestMigrationVersionsAreUniqueAndOrdered(t *testing.T) {
	seen := map[int]string{}
	prev := 0
	for i, m := range migrations {
		if m.version <= 0 {
			t.Errorf("migration %d (%s) has no version", i, m.name)
		}
		if other, dup := seen[m.version]; dup {
			t.Errorf("version %d is used twice: %q and %q — the later one is SKIPPED on "+
				"any database that already applied the earlier, so its schema change "+
				"silently never happens", m.version, other, m.name)
		}
		seen[m.version] = m.name
		if m.version <= prev {
			t.Errorf("migration %d (%s) has version %d, not above the previous %d; the "+
				"list is applied in order and must ascend", i, m.name, m.version, prev)
		}
		prev = m.version
		if m.name == "" {
			t.Errorf("migration version %d has no name, so a failure names nothing", m.version)
		}
	}
}

func TestEveryMigrationHasBothBackends(t *testing.T) {
	for _, m := range migrations {
		if len(m.sqlite) == 0 {
			t.Errorf("version %d (%s) has no sqlite statements", m.version, m.name)
		}
		if len(m.postgres) == 0 {
			t.Errorf("version %d (%s) has no postgres statements", m.version, m.name)
		}
	}
}

// Compares the names of the tables and indexes each dialect creates; column
// types legitimately differ, the set of objects must not.
func TestBothDialectsCreateTheSameObjects(t *testing.T) {
	created := regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?(TABLE|INDEX)\s+(?:IF NOT EXISTS\s+)?([a-z_0-9]+)`)
	names := func(stmts []string) map[string]bool {
		out := map[string]bool{}
		for _, s := range stmts {
			for _, m := range created.FindAllStringSubmatch(s, -1) {
				out[strings.ToLower(m[1])+" "+m[2]] = true
			}
		}
		return out
	}

	for _, m := range migrations {
		lite, pg := names(m.sqlite), names(m.postgres)
		for obj := range lite {
			if !pg[obj] {
				t.Errorf("version %d (%s) creates %q on SQLite but not on Postgres — the two backends "+
					"would end up with different schemas and only one of them is production",
					m.version, m.name, obj)
			}
		}
		for obj := range pg {
			if !lite[obj] {
				t.Errorf("version %d (%s) creates %q on Postgres but not on SQLite — the single-binary "+
					"path would be missing it", m.version, m.name, obj)
			}
		}
	}
}

// A version that carries a different name on main was numbered independently
// on two branches. Skipped when git or main is unavailable.
func TestNoVersionCollidesWithMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable, so the cross-branch comparison cannot be made")
	}
	out, err := exec.Command("git", "show", "main:internal/store/migrations.go").Output()
	if err != nil {
		t.Skip("no main branch to compare against")
	}

	mine := map[int]string{}
	for _, m := range migrations {
		mine[m.version] = m.name
	}
	for version, name := range parseMigrationVersions(string(out)) {
		ours, ok := mine[version]
		if !ok || ours == name {
			continue
		}
		t.Errorf("version %d is %q here and %q on main — main is what production runs, "+
			"so this migration arrives at a database that has already passed %d and is "+
			"SKIPPED. The table is never created and the deploy still reports success. "+
			"Pick a number above every version on main.", version, ours, name, version)
	}
}

// parseMigrationVersions pulls version/name pairs out of migrations.go source
// from another branch, which cannot be compiled into this binary.
func parseMigrationVersions(src string) map[int]string {
	out := map[int]string{}
	verRe := regexp.MustCompile(`version:\s*(\d+)`)
	nameRe := regexp.MustCompile(`name:\s*"([^"]*)"`)
	for _, loc := range verRe.FindAllStringSubmatchIndex(src, -1) {
		v, err := strconv.Atoi(src[loc[2]:loc[3]])
		if err != nil {
			continue
		}
		rest := src[loc[1]:]
		if m := nameRe.FindStringSubmatch(rest); m != nil {
			out[v] = m[1]
		}
	}
	return out
}

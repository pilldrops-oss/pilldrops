package store

import "testing"

// A row written the way every row was written before migration 60 — the old
// column list, no visibility named — must come back org-visible and owned by
// nobody. Anything else is a silent mass-hide of every sandbox in production.
func TestMigrationV60LeavesExistingSandboxesOrgVisible(t *testing.T) {
	st := openTemp(t)
	if _, err := st.exec(t.Context(),
		`INSERT INTO sandboxes (id, template, org_id, state, created_at, expires_at, updated_at, env, networks)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sb-legacy", "base", "u:cust1", "running", int64(1), int64(0), int64(1), "{}", "[]",
	); err != nil {
		t.Fatalf("insert a pre-visibility row: %v", err)
	}
	var createdBy, visibility string
	if err := st.queryRow(t.Context(),
		`SELECT created_by, visibility FROM sandboxes WHERE id = ?`, "sb-legacy",
	).Scan(&createdBy, &visibility); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if createdBy != "" || visibility != "org" {
		t.Fatalf("pre-visibility row reads (created_by=%q, visibility=%q), want (\"\", \"org\")", createdBy, visibility)
	}

	recs, err := st.SandboxesByOrg(t.Context(), "u:cust1")
	if err != nil || len(recs) != 1 {
		t.Fatalf("SandboxesByOrg = %d rows, err=%v", len(recs), err)
	}
	if recs[0].Visibility != "org" {
		t.Fatalf("record visibility = %q, want org", recs[0].Visibility)
	}

	var name string
	if err := st.queryRow(t.Context(), `SELECT name FROM schema_migrations WHERE version = 60`).Scan(&name); err != nil || name != "sandbox_visibility" {
		t.Fatalf("v60 not recorded (name=%q err=%v)", name, err)
	}
}

// created_by and visibility survive a save that does not carry them: the
// daemon's reconciler writes the whole record, but an adopted or partially
// built one must not hand a private sandbox to the org.
func TestSaveSandboxKeepsVisibilityWhenTheRecordOmitsIt(t *testing.T) {
	st := openTemp(t)
	rec := SandboxRecord{
		ID: "sb-1", Template: "base", OrgID: "u:cust1", State: "running",
		CreatedBy: "key_a", Visibility: "private",
		CreatedAt: 1, UpdatedAt: 1,
	}
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	blank := rec
	blank.CreatedBy, blank.Visibility = "", ""
	blank.State = "paused"
	if err := st.SaveSandbox(t.Context(), blank); err != nil {
		t.Fatalf("resave: %v", err)
	}
	recs, err := st.SandboxesByOrg(t.Context(), "u:cust1")
	if err != nil || len(recs) != 1 {
		t.Fatalf("SandboxesByOrg = %d rows, err=%v", len(recs), err)
	}
	if recs[0].CreatedBy != "key_a" || recs[0].Visibility != "private" {
		t.Fatalf("after a save that named neither: (created_by=%q, visibility=%q), want (key_a, private)",
			recs[0].CreatedBy, recs[0].Visibility)
	}
}

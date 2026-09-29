package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"
)

func TestUsersTableCreatedByMigration(t *testing.T) {
	st := openTemp(t)
	if err := st.UpsertLogin(t.Context(), 1000, "u:abc", "abc", "a@b.com"); err != nil {
		t.Fatalf("users table missing / upsert failed: %v", err)
	}
}

func TestGetUserDetail(t *testing.T) {
	st := openTemp(t)
	now := int64(12000)
	if err := st.UpsertLogin(t.Context(), now, "u:abc", "abc", "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "s1", OrgID: "u:abc", Template: "python-3.11", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetUserDetail(t.Context(), "u:abc")
	if err != nil {
		t.Fatalf("GetUserDetail: %v", err)
	}
	if d == nil {
		t.Fatal("want detail, got nil")
	}
	if d.User.OwnerID != "u:abc" || len(d.Active) != 1 {
		t.Fatalf("bad detail: user=%+v active=%d", d.User, len(d.Active))
	}
	// No attribution set, so the sandbox lands in "unknown".
	if d.ByClient["unknown"] != 1 {
		t.Fatalf("ByClient = %+v, want unknown:1", d.ByClient)
	}
	missing, err := st.GetUserDetail(t.Context(), "u:nope")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if missing != nil {
		t.Fatalf("want nil for unknown owner, got %+v", missing)
	}
}

// A CreationsByClient error must degrade to an empty ByClient map, not fail the drill-down.
func TestGetUserDetailTolerantOfCreationsByClientFailure(t *testing.T) {
	st := openTemp(t)
	now := int64(31000)
	if err := st.UpsertLogin(t.Context(), now, "u:broken", "broken", "b@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-broken", OrgID: "u:broken", Template: "node", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Only CreationsByClient reads this column.
	if _, err := st.exec(t.Context(), `ALTER TABLE sandboxes DROP COLUMN client`); err != nil {
		t.Fatalf("drop column (test setup): %v", err)
	}
	// Postgres runs share one database, so the schema must be repaired for later tests.
	t.Cleanup(func() {
		if _, err := st.exec(context.Background(),
			`ALTER TABLE sandboxes ADD COLUMN client TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Errorf("restore sandboxes.client: %v", err)
		}
	})

	d, err := st.GetUserDetail(t.Context(), "u:broken")
	if err != nil {
		t.Fatalf("GetUserDetail should tolerate CreationsByClient failure, got err: %v", err)
	}
	if d == nil {
		t.Fatal("want detail, got nil")
	}
	if d.ByClient == nil || len(d.ByClient) != 0 {
		t.Fatalf("ByClient = %+v, want empty (non-nil) map on tolerated failure", d.ByClient)
	}
}

// The orchestrator has no SANDBOXD_SECRET_KEY, so GetUserDetail must never decrypt env.
func TestGetUserDetailDoesNotRequireSecretKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	withKey, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := withKey.SetSecretKey(hex.EncodeToString(bytes.Repeat([]byte{3}, 32))); err != nil {
		t.Fatalf("set key: %v", err)
	}
	now := int64(50000)
	if err := withKey.UpsertLogin(t.Context(), now, "u:sealed", "sealed", "s@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := withKey.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-sealed", OrgID: "u:sealed", Template: "python-3.11", State: "running",
		Env: `{"DB_PASSWORD":"hunter2"}`, CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	withKey.Close()

	noKey, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen without key: %v", err)
	}
	t.Cleanup(func() { noKey.Close() })

	d, err := noKey.GetUserDetail(t.Context(), "u:sealed")
	if err != nil {
		t.Fatalf("GetUserDetail must not need SANDBOXD_SECRET_KEY (env is unused by the god-view): %v", err)
	}
	if d == nil {
		t.Fatal("want detail, got nil")
	}
	if len(d.Active) != 1 || d.Active[0].ID != "sb-sealed" || d.Active[0].State != "running" {
		t.Fatalf("active = %+v", d.Active)
	}
}

func TestListUsersAggregates(t *testing.T) {
	st := openTemp(t)
	now := int64(9000)
	if err := st.UpsertLogin(t.Context(), now, "u:abc", "abc", "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertLogin(t.Context(), now-100, "u:xyz", "xyz", "x@y.com"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2"} {
		if err := st.SaveSandbox(t.Context(), SandboxRecord{
			ID: id, OrgID: "u:abc", Template: "python-3.11", State: "running",
			CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveFunction(t.Context(), FunctionRecord{
		Name: "hello", OrgID: "u:abc", Template: "python-3.11", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlan(t.Context(), "u:abc", "pro"); err != nil {
		t.Fatal(err)
	}

	users, err := st.ListUsers(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("want 2 users, got %d", len(users))
	}
	abc := users[0]
	if abc.OwnerID != "u:abc" {
		t.Fatalf("want u:abc first, got %s", abc.OwnerID)
	}
	if abc.ActiveSandboxes != 2 || abc.TotalCreated != 2 || abc.Functions != 1 {
		t.Fatalf("bad aggregates: %+v", abc)
	}
	if abc.Plan != "pro" {
		t.Fatalf("want plan pro, got %q", abc.Plan)
	}
}

func TestSandboxesByOrg(t *testing.T) {
	st := openTemp(t)
	now := int64(5000)
	save := func(id, org string) {
		if err := st.SaveSandbox(t.Context(), SandboxRecord{
			ID: id, OrgID: org, Template: "python-3.11", State: "running",
			CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("s1", "u:abc")
	save("s2", "u:abc")
	save("s3", "u:xyz")
	got, err := st.SandboxesByOrg(t.Context(), "u:abc")
	if err != nil {
		t.Fatalf("SandboxesByOrg: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sandboxes for u:abc, got %d", len(got))
	}
}

func TestUpsertLoginSessionGap(t *testing.T) {
	st := openTemp(t)
	mustLogin := func(now int64) {
		if err := st.UpsertLogin(t.Context(), now, "u:abc", "abc", "a@b.com"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	get := func() UserRow {
		rows, err := st.ListUsers(t.Context(), 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("want 1 user, got %d", len(rows))
		}
		return rows[0]
	}
	mustLogin(1000)
	if u := get(); u.LoginCount != 1 || u.FirstSeen != 1000 || u.LastSeen != 1000 {
		t.Fatalf("after first: %+v", u)
	}
	mustLogin(1000 + 1000)
	if u := get(); u.LoginCount != 1 || u.LastSeen != 2000 {
		t.Fatalf("within gap should not count: %+v", u)
	}
	mustLogin(2000 + 2000)
	if u := get(); u.LoginCount != 2 || u.LastSeen != 4000 || u.FirstSeen != 1000 {
		t.Fatalf("beyond gap should count: %+v", u)
	}
}

// TotalCreated counts active sandboxes plus history rows.
func TestListUsersHistoryAggregation(t *testing.T) {
	st := openTemp(t)
	now := int64(20000)
	if err := st.UpsertLogin(t.Context(), now, "u:abc", "abc", "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sa1", OrgID: "u:abc", Template: "python-3.11", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "sh1", OrgID: "u:abc", Template: "python-3.11",
		ExitState:    "destroyed",
		CreatedAt:    now - 600,
		DestroyedAt:  now,
		DurationSecs: 600,
	}); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}

	users, err := st.ListUsers(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %d", len(users))
	}
	abc := users[0]
	if abc.ActiveSandboxes != 1 {
		t.Fatalf("want ActiveSandboxes=1, got %d", abc.ActiveSandboxes)
	}
	if abc.TotalCreated != abc.ActiveSandboxes+1 {
		t.Fatalf("want TotalCreated=%d (active+1 history), got %d", abc.ActiveSandboxes+1, abc.TotalCreated)
	}
}

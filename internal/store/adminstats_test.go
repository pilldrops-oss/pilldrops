package store

import (
	"testing"
)

func TestAdminOverview(t *testing.T) {
	st := openTemp(t)
	now := int64(2_000_000)

	if err := st.UpsertLogin(t.Context(), now, "u:abc", "abc", "a@b.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertLogin(t.Context(), now-10_000_000, "u:old", "old", "o@b.com"); err != nil { // inactive (>24h)
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sa1", OrgID: "u:abc", Template: "python-3.11", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "h1", OrgID: "u:abc", Template: "node", ExitState: "exited",
		CreatedAt: now - 600, DestroyedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHistory(t.Context(), HistoryRecord{
		ID: "h2", OrgID: "u:abc", Template: "node", ExitState: "crashed",
		CreatedAt: now - 1200, DestroyedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordEvent(t.Context(), EventRecord{
		OrgID: "u:abc", SandboxID: "sa1", Event: "created", Template: "python-3.11", ActiveAfter: 1, At: now,
	}); err != nil {
		t.Fatal(err)
	}

	o, err := st.AdminOverview(t.Context(), now, 14)
	if err != nil {
		t.Fatalf("AdminOverview: %v", err)
	}
	if o.TotalUsers != 2 {
		t.Fatalf("TotalUsers=%d want 2", o.TotalUsers)
	}
	if o.ActiveUsers24h != 1 {
		t.Fatalf("ActiveUsers24h=%d want 1", o.ActiveUsers24h)
	}
	if o.ActiveSandboxes != 1 {
		t.Fatalf("ActiveSandboxes=%d want 1", o.ActiveSandboxes)
	}
	if o.TotalCreated != 3 { // 1 active + 2 history
		t.Fatalf("TotalCreated=%d want 3", o.TotalCreated)
	}
	if o.CrashedCount != 1 {
		t.Fatalf("CrashedCount=%d want 1", o.CrashedCount)
	}
	if len(o.Templates) == 0 {
		t.Fatal("want a templates breakdown")
	}
	if len(o.ExitStates) == 0 {
		t.Fatal("want an exit-states breakdown")
	}
	if len(o.CreatedByDay) == 0 {
		t.Fatal("want a created-by-day series")
	}
	if len(o.RecentErrors) != 1 {
		t.Fatalf("RecentErrors=%d want 1", len(o.RecentErrors))
	}
	if len(o.RecentActivity) != 1 {
		t.Fatalf("RecentActivity=%d want 1", len(o.RecentActivity))
	}
}

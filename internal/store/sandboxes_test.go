package store

import "testing"

func TestGetActiveSandboxRow(t *testing.T) {
	st := openTemp(t)
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-1", Template: "base", OrgID: "org-a", State: "running",
		VCPU: 2, MemMiB: 1024, CreatedAt: 10, ExpiresAt: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSandboxAttribution(t.Context(), "sb-1", "human", "web"); err != nil {
		t.Fatal(err)
	}

	r, ok, err := st.GetActiveSandboxRow(t.Context(), "sb-1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.OrgID != "org-a" || r.Source != "human" || r.Template != "base" || r.VCPU != 2 ||
		r.ID != "sb-1" || r.State != "running" || r.CreatedAt != 10 || r.ExpiresAt != 20 {
		t.Fatalf("row = %+v", r)
	}
	if _, ok, err := st.GetActiveSandboxRow(t.Context(), "nope"); ok || err != nil {
		t.Fatalf("unknown id: ok=%v err=%v (want false, nil)", ok, err)
	}
}

func TestActiveSandboxRowsByOrg(t *testing.T) {
	st := openTemp(t)
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-a1", Template: "base", OrgID: "org-a", State: "running",
		VCPU: 2, MemMiB: 1024, CreatedAt: 10, ExpiresAt: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-a2", Template: "base", OrgID: "org-a", State: "paused",
		VCPU: 1, MemMiB: 512, CreatedAt: 20, ExpiresAt: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-b1", Template: "base", OrgID: "org-b", State: "running",
		CreatedAt: 5, ExpiresAt: 15,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ActiveSandboxRowsByOrg(t.Context(), "org-a")
	if err != nil {
		t.Fatalf("ActiveSandboxRowsByOrg: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows for org-a, got %+v", rows)
	}
	if rows[0].ID != "sb-a2" || rows[1].ID != "sb-a1" {
		t.Fatalf("want DESC by created_at, got %+v", rows)
	}
	for _, r := range rows {
		if r.OrgID != "org-a" {
			t.Fatalf("org-scoping leaked another org's row: %+v", r)
		}
	}

	empty, err := st.ActiveSandboxRowsByOrg(t.Context(), "org-unknown")
	if err != nil {
		t.Fatalf("unknown org: %v", err)
	}
	if empty == nil {
		t.Fatal("want non-nil empty slice for unknown org")
	}
	if len(empty) != 0 {
		t.Fatalf("want 0 rows for unknown org, got %d", len(empty))
	}
}

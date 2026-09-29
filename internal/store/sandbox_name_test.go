package store

import "testing"

func TestSandboxNameRoundTrips(t *testing.T) {
	st := openTemp(t)

	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb_named", Template: "playwright", OrgID: "default", State: "running",
		Name: "nightly regression", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}

	rows, err := st.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("LoadSandboxes: %v", err)
	}
	var got *SandboxRecord
	for i := range rows {
		if rows[i].ID == "sb_named" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("sandbox not found")
	}
	if got.Name != "nightly regression" {
		t.Fatalf("Name = %q, want %q", got.Name, "nightly regression")
	}
}

func TestSandboxWithoutANameIsEmptyNotNull(t *testing.T) {
	st := openTemp(t)

	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb_plain", Template: "base", OrgID: "default", State: "running",
		CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}

	rows, err := st.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("LoadSandboxes: %v", err)
	}
	for _, r := range rows {
		if r.ID == "sb_plain" && r.Name != "" {
			t.Fatalf("Name = %q, want empty", r.Name)
		}
	}
}

// The name must survive the ON CONFLICT update clause, not be ignored by it.
func TestSandboxNameCanBeChangedAndCleared(t *testing.T) {
	st := openTemp(t)
	base := SandboxRecord{
		ID: "sb_rename", Template: "base", OrgID: "default", State: "running",
		Name: "first", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	}
	if err := st.SaveSandbox(t.Context(), base); err != nil {
		t.Fatalf("save: %v", err)
	}

	base.Name = "second"
	if err := st.SaveSandbox(t.Context(), base); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n := nameOf(t, st, "sb_rename"); n != "second" {
		t.Fatalf("after rename Name = %q, want %q", n, "second")
	}

	base.Name = ""
	if err := st.SaveSandbox(t.Context(), base); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := nameOf(t, st, "sb_rename"); n != "" {
		t.Fatalf("after clear Name = %q, want empty", n)
	}
}

func nameOf(t *testing.T, st *Store, id string) string {
	t.Helper()
	rows, err := st.LoadSandboxes(t.Context())
	if err != nil {
		t.Fatalf("LoadSandboxes: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	t.Fatalf("sandbox %q not found", id)
	return ""
}

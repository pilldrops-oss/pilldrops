package store

import "testing"

func TestPublicPreviewRoundTrip(t *testing.T) {
	st := openTemp(t)

	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb-1", Template: "python-3.11", OrgID: "org1", State: "running",
		CreatedAt: 1, ExpiresAt: 0, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Default is private.
	if pub, err := st.IsPublicPreview(t.Context(), "sb-1"); err != nil || pub {
		t.Fatalf("default: pub=%v err=%v, want false/nil", pub, err)
	}
	if err := st.SetPublicPreview(t.Context(), "sb-1", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if pub, err := st.IsPublicPreview(t.Context(), "sb-1"); err != nil || !pub {
		t.Fatalf("after set: pub=%v err=%v, want true/nil", pub, err)
	}
	if err := st.SetPublicPreview(t.Context(), "sb-1", false); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if pub, _ := st.IsPublicPreview(t.Context(), "sb-1"); pub {
		t.Error("want private after unset")
	}

	// An unknown sandbox is private, not an error.
	if pub, err := st.IsPublicPreview(t.Context(), "sb-nonexistent"); err != nil || pub {
		t.Errorf("unknown id: pub=%v err=%v, want false/nil", pub, err)
	}
}

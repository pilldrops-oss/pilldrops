package store

import "testing"

// The identity hash is the sandbox's whole credential: the row resolves it,
// an empty hash resolves nothing, and deleting the row revokes it.
func TestSandboxByIdentityHash(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sb-a", Template: "base", OrgID: "org-a", State: "running", IdentityHash: "aaaa"}); err != nil {
		t.Fatal(err)
	}
	// A sandbox with no identity: its '' must never be a match for a '' probe.
	if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sb-none", Template: "base", OrgID: "org-a", State: "running"}); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := st.SandboxByIdentityHash(ctx, "aaaa")
	if err != nil || !ok || rec.ID != "sb-a" || rec.OrgID != "org-a" || rec.IdentityHash != "aaaa" {
		t.Fatalf("resolve: ok=%v err=%v rec=%+v", ok, err, rec)
	}
	for _, probe := range []string{"", "bbbb"} {
		if _, ok, err := st.SandboxByIdentityHash(ctx, probe); ok || err != nil {
			t.Fatalf("probe %q: ok=%v err=%v (want not found)", probe, ok, err)
		}
	}
	// Re-saving the row (the daemon upserts on every state change) keeps the hash.
	rec.State = "paused"
	if err := st.SaveSandbox(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.SandboxByIdentityHash(ctx, "aaaa"); !ok {
		t.Fatal("the hash did not survive an upsert of the same row")
	}
	// Destroy = delete row = revoke.
	if err := st.DeleteSandbox(ctx, "sb-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.SandboxByIdentityHash(ctx, "aaaa"); ok || err != nil {
		t.Fatalf("after delete: ok=%v err=%v (want not found)", ok, err)
	}
}

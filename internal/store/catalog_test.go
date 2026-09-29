package store

import "testing"

func TestCountPersistentForOrgCountsWorkstationsFleetWide(t *testing.T) {
	st := openTemp(t)

	for _, r := range []SandboxRecord{
		{ID: "ws-node-1", Template: "base", OrgID: "org-a", State: "running", CreatedAt: 1, ExpiresAt: 0},
		{ID: "ws-node-2", Template: "base", OrgID: "org-a", State: "paused", CreatedAt: 2, ExpiresAt: 0},
		// A lease expiry means ephemeral, not a workstation.
		{ID: "lease-1", Template: "base", OrgID: "org-a", State: "running", CreatedAt: 3, ExpiresAt: 9999},
		{ID: "ws-gone", Template: "base", OrgID: "org-a", State: "destroyed", CreatedAt: 4, ExpiresAt: 0},
		{ID: "ws-other", Template: "base", OrgID: "org-b", State: "running", CreatedAt: 5, ExpiresAt: 0},
	} {
		if err := st.SaveSandbox(t.Context(), r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}

	n, err := st.CountPersistentForOrg(t.Context(), "org-a")
	if err != nil {
		t.Fatalf("CountPersistentForOrg: %v — an erroring count fails open, so the cap never fires", err)
	}
	if n != 2 {
		t.Errorf("org-a has %d persistent sandboxes, want 2 (running + paused, excluding the lease and the destroyed one)", n)
	}
	if n, err := st.CountPersistentForOrg(t.Context(), "org-b"); err != nil || n != 1 {
		t.Errorf("org-b = %d (err %v), want 1 — orgs must be counted separately", n, err)
	}
	if n, err := st.CountPersistentForOrg(t.Context(), "org-nobody"); err != nil || n != 0 {
		t.Errorf("org with nothing running = %d (err %v), want 0", n, err)
	}
}

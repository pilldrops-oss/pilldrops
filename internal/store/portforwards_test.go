package store

import (
	"sync"
	"testing"
)

func TestPortForwardUpsertIsStableAndDestroyFreesThePort(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.SaveSandbox(ctx, SandboxRecord{
		ID: "sb-1", Template: "base", OrgID: "org1", NodeID: "n1", State: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save sandbox: %v", err)
	}

	first, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 5432, TCP: true, CreatedAt: 10}, 40000, 60000)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if first.PublicPort < 40000 || first.PublicPort > 60000 {
		t.Fatalf("public port %d is outside the range", first.PublicPort)
	}

	// Re-expose: same public port, new protocol set. Anything else breaks a
	// published endpoint the customer already handed out.
	again, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 5432, TCP: true, UDP: true, CreatedAt: 20}, 40000, 60000)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if again.PublicPort != first.PublicPort {
		t.Fatalf("re-expose moved the public port: %d then %d", first.PublicPort, again.PublicPort)
	}
	if !again.UDP || !again.TCP {
		t.Fatalf("protocol set not updated: %+v", again)
	}
	if n, err := st.CountPortForwards(ctx, "sb-1"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v; re-expose created a second row", n, err)
	}

	// The active listing carries the node so the ingress knows where to dial.
	active, err := st.ActivePortForwards(ctx)
	if err != nil || len(active) != 1 || active[0].NodeID != "n1" {
		t.Fatalf("active = %+v, %v", active, err)
	}

	// Destroy removes the rows and frees the port (no PRAGMA foreign_keys, so
	// this is DeleteSandbox's own DELETE, not a cascade).
	if err := st.DeleteSandbox(ctx, "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if rows, err := st.PortForwardsBySandbox(ctx, "sb-1"); err != nil || len(rows) != 0 {
		t.Fatalf("after destroy rows = %+v, %v", rows, err)
	}
	// Positive control: the freed port is allocatable again in a one-port range.
	if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sb-2", Template: "base", OrgID: "org1", State: "running", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-2", GuestPort: 80, TCP: true}, first.PublicPort, first.PublicPort); err != nil || got.PublicPort != first.PublicPort {
		t.Fatalf("freed port not reusable: %+v, %v", got, err)
	}
}

// Fifty concurrent allocations must never collide: the UNIQUE index on
// public_port is the lock, and a loser retries with a different port.
func TestPortForwardAllocationNeverRepeatsAPublicPort(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sb-1", Template: "base", OrgID: "o", State: "running", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	const n = 50
	// A 100-port range, so a collision is likely enough that a missing lock shows.
	const lo, hi = 41000, 41099
	var wg sync.WaitGroup
	got := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 1000 + i, TCP: true}, lo, hi)
			got[i], errs[i] = f.PublicPort, err
		}(i)
	}
	wg.Wait()

	seen := map[int]int{}
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("upsert %d: %v", i, errs[i])
		}
		if prev, dup := seen[got[i]]; dup {
			t.Fatalf("public port %d handed to guest ports %d and %d", got[i], 1000+prev, 1000+i)
		}
		seen[got[i]] = i
	}
	if len(seen) != n {
		t.Fatalf("%d distinct public ports for %d forwards", len(seen), n)
	}
}

func TestPortForwardRangeExhaustionIsNamed(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	if err := st.SaveSandbox(ctx, SandboxRecord{ID: "sb-1", Template: "base", OrgID: "o", State: "running", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 1, TCP: true}, 45000, 45000); err != nil {
		t.Fatal(err)
	}
	_, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 2, TCP: true}, 45000, 45000)
	if err == nil {
		t.Fatal("a full range allocated a second port")
	}
	if ok, _ := st.DeletePortForward(ctx, "sb-1", 1); !ok {
		t.Fatal("delete reported no row")
	}
	if _, err := st.UpsertPortForward(ctx, PortForward{SandboxID: "sb-1", GuestPort: 2, TCP: true}, 45000, 45000); err != nil {
		t.Fatalf("after the delete the range is free again: %v", err)
	}
}

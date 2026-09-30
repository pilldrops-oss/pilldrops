//go:build linux

package vm

import "testing"

func TestUidForSandbox_DistinctForPoolIDs(t *testing.T) {
	a := uidForSandbox("pool-9705a5a35c39")
	b := uidForSandbox("pool-49c99593c204")
	if a == b {
		t.Fatalf("distinct pool ids must map to distinct uids, both = %d", a)
	}
	for _, u := range []int{a, b} {
		if u < 10000 || u >= 60000 {
			t.Fatalf("uid %d out of range [10000,60000)", u)
		}
	}
}

func TestUidForSandbox_Deterministic(t *testing.T) {
	first := uidForSandbox("sb_1784_abc")
	if again := uidForSandbox("sb_1784_abc"); again != first {
		t.Fatalf("got %d then %d: "+"must be deterministic for the same id (pause/resume ownership depends on it)", first, again)
	}
}

// vmUID(true) must agree with uidForSandbox so the overlay chown, tap owner and
// jailer uid never disagree. The default lives in nodeboot, not here.
func TestVmUID(t *testing.T) {
	const id = "sb-test-nonroot-0001"

	if got := vmUID(false, id); got != 0 {
		t.Fatalf("vmUID(false, ...) = %d, want 0 (the FC_NONROOT_VMM=0 break-glass)", got)
	}

	got := vmUID(true, id)
	want := uidForSandbox(id)
	if got != want {
		t.Fatalf("vmUID(true, ...) = %d, want %d (uidForSandbox(id))", got, want)
	}
	if got < 10000 || got >= 60000 {
		t.Fatalf("vmUID(true, ...) = %d out of range [10000,60000)", got)
	}
}

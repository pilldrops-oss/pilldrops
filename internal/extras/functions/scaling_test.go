package functions

import "testing"

func TestLeastLoaded(t *testing.T) {
	reps := []*replica{
		{sandboxID: "a", inflight: 3},
		{sandboxID: "b", inflight: 1},
		{sandboxID: "c", inflight: 2},
	}
	if got := leastLoaded(reps, targetConcurrency); got == nil || got.sandboxID != "b" {
		t.Fatalf("leastLoaded = %v, want b", got)
	}
	if got := leastLoaded(reps, 1); got != nil {
		t.Fatalf("leastLoaded(limit=1) = %v, want nil", got)
	}
	if got := leastLoaded(nil, targetConcurrency); got != nil {
		t.Fatalf("leastLoaded(nil) = %v, want nil", got)
	}
}

package cluster

import (
	"testing"
	"time"
)

func TestRegistryUpsertAndHealth(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	upsert(r, "n1", "http://n1:18080", "k1", "fsn1", map[string]string{"gpu": "no"}, "1.0", Capacity{MemTotalMB: 64000, MemAvailMB: 40000}, now)
	upsert(r, "n2", "http://n2:18080", "k2", "fsn1", nil, "1.0", Capacity{MemTotalMB: 8000, MemAvailMB: 500}, now.Add(-time.Minute))

	if len(r.Nodes()) != 2 {
		t.Fatalf("Nodes = %d, want 2", len(r.Nodes()))
	}
	healthy := r.Healthy(now)
	if len(healthy) != 1 || healthy[0].ID != "n1" {
		t.Fatalf("Healthy = %v, want [n1]", healthy)
	}
	upsert(r, "n2", "http://n2:18080", "", "fsn1", nil, "1.0", Capacity{MemTotalMB: 8000, MemAvailMB: 7000}, now)
	if len(r.Healthy(now)) != 2 {
		t.Fatalf("after re-heartbeat, healthy = %d, want 2", len(r.Healthy(now)))
	}
	if n, _ := r.Node("n2"); n.Cap.MemAvailMB != 7000 || n.Key != "k2" {
		t.Fatalf("n2 not refreshed / key lost: %+v", n)
	}
}

func TestRegistryPlacement(t *testing.T) {
	r := NewRegistry()
	upsert(r, "n1", "http://n1", "k1", "default", nil, "1.0", Capacity{}, time.Now())
	r.Place("sb-1", "n1")
	if n, ok := r.NodeFor("sb-1"); !ok || n.ID != "n1" {
		t.Fatalf("NodeFor(sb-1) = %v,%v want n1", n, ok)
	}
	r.Unplace("sb-1")
	if _, ok := r.NodeFor("sb-1"); ok {
		t.Fatal("expected sb-1 unplaced")
	}
	r.Place("sb-2", "gone")
	if _, ok := r.NodeFor("sb-2"); ok {
		t.Fatal("expected not-found for vanished node")
	}
}

// A wiped route catalog must not make live sandboxes read as not found: the
// fallback names the node, and the route is written back.
func TestNodeForFallsBackAfterRouteWipe(t *testing.T) {
	r := NewRegistry()
	upsert(r, "n1", "http://n1", "k1", "default", nil, "1.0", Capacity{}, time.Now())
	asked := 0
	r.SetRouteFallback(func(id string) (string, bool) {
		asked++
		return "n1", id == "sb-1"
	})
	if n, ok := r.NodeFor("sb-1"); !ok || n.ID != "n1" {
		t.Fatalf("NodeFor(sb-1) = %v,%v want n1 from the fallback", n, ok)
	}
	if _, ok := r.NodeFor("sb-1"); !ok || asked != 1 {
		t.Fatalf("the fallback's answer was not written back as a route (asked %d times)", asked)
	}
	if _, ok := r.NodeFor("sb-unknown"); ok {
		t.Fatal("an id the fallback does not know was placed")
	}
}

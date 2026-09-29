package cluster

import (
	"os"
	"sort"
	"testing"
	"time"
)

// eachRouteStore runs fn against the in-memory store and, when
// SANDBOXD_TEST_REDIS_URL is set, the Redis store too.
func eachRouteStore(t *testing.T, fn func(t *testing.T, rs RouteStore)) {
	t.Run("mem", func(t *testing.T) { fn(t, newMemRouteStore()) })

	if url := os.Getenv("SANDBOXD_TEST_REDIS_URL"); url != "" {
		rs, err := NewRedisRouteStore(url)
		if err != nil {
			t.Fatalf("redis route store: %v", err)
		}
		if err := rs.reset(); err != nil {
			t.Fatalf("reset: %v", err)
		}
		t.Run("redis", func(t *testing.T) { fn(t, rs) })
	}
}

func TestRouteStore(t *testing.T) {
	eachRouteStore(t, func(t *testing.T, rs RouteStore) {
		if _, ok, err := rs.NodeFor("nope"); err != nil || ok {
			t.Fatalf("empty NodeFor: ok=%v err=%v", ok, err)
		}

		mustNil(t, rs.Place("sb1", "node-a"))
		mustNil(t, rs.Place("sb2", "node-a"))
		mustNil(t, rs.Place("sb3", "node-b"))

		if id, ok, err := rs.NodeFor("sb1"); err != nil || !ok || id != "node-a" {
			t.Fatalf("NodeFor(sb1) = %q ok=%v err=%v, want node-a", id, ok, err)
		}

		on, err := rs.PlacementsOn("node-a")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(on)
		if len(on) != 2 || on[0] != "sb1" || on[1] != "sb2" {
			t.Fatalf("PlacementsOn(node-a) = %v, want [sb1 sb2]", on)
		}

		mustNil(t, rs.Unplace("sb1"))
		if _, ok, _ := rs.NodeFor("sb1"); ok {
			t.Fatal("sb1 still placed after Unplace")
		}

		n, err := rs.UnplaceNode("node-a")
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("UnplaceNode(node-a) removed %d, want 1", n)
		}
		if _, ok, _ := rs.NodeFor("sb2"); ok {
			t.Fatal("sb2 still placed after UnplaceNode(node-a)")
		}
		if id, ok, _ := rs.NodeFor("sb3"); !ok || id != "node-b" {
			t.Fatalf("sb3 should still be on node-b, got %q ok=%v", id, ok)
		}
	})
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistrySharedRoutesAcrossReplicas(t *testing.T) {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set SANDBOXD_TEST_REDIS_URL to run the shared-route test")
	}
	rs, err := NewRedisRouteStore(url)
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	mustNil(t, rs.reset())

	newReplica := func() *Registry {
		s, err := NewRedisRouteStore(url)
		if err != nil {
			t.Fatalf("redis: %v", err)
		}
		return NewRegistryWithRoutes(s)
	}
	now := time.Now()

	regA := newReplica()
	regB := newReplica()
	// The node table is per-replica here, so each learns the node itself.
	upsert(regA, "node-a", "http://x", "k", "", nil, "", Capacity{}, now)
	upsert(regB, "node-a", "http://x", "k", "", nil, "", Capacity{}, now)

	regA.Place("sb1", "node-a")
	if n, ok := regB.NodeFor("sb1"); !ok || n.ID != "node-a" {
		t.Fatalf("replica B could not resolve replica A's placement (ok=%v)", ok)
	}

	regC := newReplica()
	upsert(regC, "node-a", "http://x", "k", "", nil, "", Capacity{}, now)
	if n, ok := regC.NodeFor("sb1"); !ok || n.ID != "node-a" {
		t.Fatal("restarted replica lost the placement — route catalog not durable")
	}
}

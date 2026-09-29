package cluster

import (
	"os"
	"testing"
	"time"
)

// eachReplicaPair runs fn against two Registry values sharing only a backend:
// in-memory always, Redis when SANDBOXD_TEST_REDIS_URL is set.
func eachReplicaPair(t *testing.T, fn func(t *testing.T, a, b *Registry)) {
	t.Helper()
	t.Run("shared-mem", func(t *testing.T) {
		ns, rs := newMemNodeStore(), newMemRouteStore()
		fn(t, NewRegistryShared(ns, rs), NewRegistryShared(ns, rs))
	})

	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Log("SANDBOXD_TEST_REDIS_URL is unset — skipping the REAL two-process case. " +
			"Set it to exercise the Redis backend (this is the one that catches serialization bugs).")
		return
	}
	t.Run("redis", func(t *testing.T) {
		nsA, err := NewRedisNodeStore(url)
		if err != nil {
			t.Fatalf("redis node store: %v", err)
		}
		if err := nsA.reset(); err != nil {
			t.Fatalf("reset: %v", err)
		}
		rsA, err := NewRedisRouteStore(url)
		if err != nil {
			t.Fatalf("redis route store: %v", err)
		}
		if err := rsA.reset(); err != nil {
			t.Fatalf("reset routes: %v", err)
		}
		nsB, err := NewRedisNodeStore(url)
		if err != nil {
			t.Fatal(err)
		}
		rsB, err := NewRedisRouteStore(url)
		if err != nil {
			t.Fatal(err)
		}
		fn(t, NewRegistryShared(nsA, rsA), NewRegistryShared(nsB, rsB))
	})
}

func beat(r *Registry, id, url, zone string, memAvail int, now time.Time) {
	upsert(r, id, url, "sf_node_"+id, zone, nil, "v1", Capacity{
		Cores: 8, MemTotalMB: 16384, MemAvailMB: memAvail,
	}, now)
}

func TestReplicaResolvesASandboxOnANodeItNeverHeardFrom(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 8000, now)
		a.Place("sb1", "node-a")

		node, ok := b.NodeFor("sb1")
		if !ok {
			t.Fatal("replica B cannot resolve a sandbox placed by replica A — " +
				"every request routed to B would 404 while the route says where it lives")
		}
		if node.URL != "http://a:18080" {
			t.Errorf("resolved URL = %q, want the node's real address", node.URL)
		}
		if node.Key != "sf_node_node-a" {
			t.Errorf("resolved node has no usable key (%q) — the forward would be rejected", node.Key)
		}
	})
}

func TestReplicaSchedulesOntoANodeItNeverHeardFrom(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 8000, now)

		got := b.PickAndReserve(Request{VCPU: 1, MemMB: 1024}, now, NodeTTL)
		if got == nil {
			t.Fatal("replica B found no capacity on a healthy fleet — " +
				"this is the 503-with-idle-nodes failure")
		}
		if got.ID != "node-a" {
			t.Errorf("picked %q, want node-a", got.ID)
		}
	})
}

func TestAReservationTakenByOneReplicaIsSeenByTheOther(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		// Exactly one 4096MB sandbox fits. Both replicas get the heartbeat so B
		// cannot fail for the unrelated reason of seeing no fleet.
		beat(a, "node-a", "http://a:18080", "eu", 5000, now)
		beat(b, "node-a", "http://a:18080", "eu", 5000, now)
		if len(b.Healthy(now)) != 1 {
			t.Fatal("precondition: replica B must be able to see the node before " +
				"we can claim the reservation is what stopped it")
		}

		if got := a.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got == nil {
			t.Fatal("replica A could not place the first sandbox")
		}
		if got := b.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got != nil {
			t.Fatalf("replica B placed a second sandbox on %s — both replicas handed out "+
				"the same capacity, which is the double-schedule this reservation exists to prevent", got.ID)
		}
	})
}

func TestReleasingAReservationFreesCapacityFleetWide(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 5000, now)

		if got := a.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got == nil {
			t.Fatal("replica A could not place")
		}
		a.Release("node-a", 4096)

		if got := b.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got == nil {
			t.Fatal("replica B still sees the released capacity as held — " +
				"a failed create would strand a node's memory until the TTL")
		}
	})
}

func TestAnAbandonedReservationExpiresForEveryReplica(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 5000, now)
		beat(b, "node-a", "http://a:18080", "eu", 5000, now)

		if got := a.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got == nil {
			t.Fatal("replica A could not place")
		}
		// B must be blocked now, or the rest would pass on a fleet where B never saw the hold.
		if got := b.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, now, NodeTTL); got != nil {
			t.Fatalf("replica B ignored a live hold on %s", got.ID)
		}
		// A dies without releasing.
		later := now.Add(ReservationTTL + time.Second)
		beat(b, "node-a", "http://a:18080", "eu", 5000, later)
		if got := b.PickAndReserve(Request{VCPU: 1, MemMB: 4096}, later, NodeTTL); got == nil {
			t.Fatal("an abandoned reservation never expired — a replica crash would " +
				"permanently strand that node's capacity")
		}
	})
}

func TestACordonOnOneReplicaIsHonouredByTheOther(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 8000, now)

		if !b.SetCordon("node-a", true) {
			t.Fatal("replica B could not cordon a node registered through replica A")
		}
		if got := a.PickAndReserve(Request{VCPU: 1, MemMB: 1024}, now, NodeTTL); got != nil {
			t.Fatalf("replica A scheduled onto cordoned node %s — the operator's "+
				"exclusion only applied to the replica that received it", got.ID)
		}
	})
}

func TestReapingUsesTheFleetsViewNotOneReplicas(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, b *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 8000, now)
		a.Place("sb1", "node-a")

		if reaped := b.ReapDeadNodes(now.Add(30*time.Second), DeadNodeTTL); len(reaped) != 0 {
			t.Fatalf("replica B reaped %v — a live node was removed and its routes "+
				"deleted because one replica had not personally seen it", reaped)
		}
		if _, ok := b.NodeFor("sb1"); !ok {
			t.Fatal("the route was destroyed by the reap")
		}

		if reaped := b.ReapDeadNodes(now.Add(DeadNodeTTL+time.Second), DeadNodeTTL); len(reaped) != 1 {
			t.Fatalf("reaped %v, want node-a once it is genuinely dead", reaped)
		}
		if _, ok := a.NodeFor("sb1"); ok {
			t.Error("routes to a reaped node survived, so requests hang instead of 404ing")
		}
	})
}

func TestAFreshReplicaServesTheFleetImmediately(t *testing.T) {
	eachReplicaPair(t, func(t *testing.T, a, _ *Registry) {
		now := time.Unix(1_700_000_000, 0).UTC()
		beat(a, "node-a", "http://a:18080", "eu", 8000, now)
		a.Place("sb1", "node-a")

		fresh := NewRegistryShared(a.nodes, a.routes)
		if _, ok := fresh.NodeFor("sb1"); !ok {
			t.Fatal("a freshly started replica cannot resolve existing sandboxes — " +
				"every deploy would 404 all sandbox traffic until heartbeats arrived")
		}
		if len(fresh.Healthy(now)) != 1 {
			t.Fatal("a freshly started replica sees an empty fleet")
		}
	})
}

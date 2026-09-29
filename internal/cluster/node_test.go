package cluster

import (
	"errors"
	"os"
	"testing"
	"time"
)

// eachNodeStore runs fn against the in-memory store and, when
// SANDBOXD_TEST_REDIS_URL is set, the Redis store too.
func eachNodeStore(t *testing.T, fn func(t *testing.T, ns NodeStore)) {
	t.Run("mem", func(t *testing.T) { fn(t, newMemNodeStore()) })

	if url := os.Getenv("SANDBOXD_TEST_REDIS_URL"); url != "" {
		ns, err := NewRedisNodeStore(url)
		if err != nil {
			t.Fatalf("redis node store: %v", err)
		}
		if err := ns.reset(); err != nil {
			t.Fatalf("reset: %v", err)
		}
		t.Run("redis", func(t *testing.T) { fn(t, ns) })
	}
}

func hb(id, url, zone string, memAvail int) NodeUpsert {
	return NodeUpsert{
		ID: id, URL: url, Key: "sf_node_" + id, Zone: zone,
		Cap: Capacity{Cores: 8, MemTotalMB: 16384, MemAvailMB: memAvail},
	}
}

func TestNodeStoreRoundTrip(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()

		if _, ok, err := ns.Get("nope"); err != nil || ok {
			t.Fatalf("empty Get: ok=%v err=%v", ok, err)
		}
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now))
		mustNil(t, ns.SetMeta("node-a", NodeMeta{Runtime: "firecracker", Isolation: true, Templates: []string{"code-interpreter"}, TagNetworks: true}))

		n, ok, err := ns.Get("node-a")
		if err != nil || !ok {
			t.Fatalf("Get(node-a): ok=%v err=%v", ok, err)
		}
		// Key is json:"-" on Node, so a naive Marshal would drop it.
		if n.Key != "sf_node_node-a" {
			t.Errorf("Key = %q, want it preserved through the store", n.Key)
		}
		if n.URL != "http://a:18080" || n.Zone != "eu" || n.Cap.MemAvailMB != 8000 {
			t.Errorf("record did not round-trip: %+v", n)
		}
		if n.Runtime != "firecracker" || !n.Isolation || len(n.Templates) != 1 || !n.TagNetworks {
			t.Errorf("meta did not round-trip: runtime=%q isolation=%v templates=%v tag_networks=%v",
				n.Runtime, n.Isolation, n.Templates, n.TagNetworks)
		}
		if !n.LastSeen.Equal(now) || !n.FirstSeen.Equal(now) {
			t.Errorf("timestamps: last=%v first=%v want both %v", n.LastSeen, n.FirstSeen, now)
		}
	})
}

func TestHeartbeatPreservesMetaAndFirstSeen(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		first := time.Unix(1_700_000_000, 0).UTC()
		later := first.Add(time.Minute)

		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), first))
		mustNil(t, ns.SetMeta("node-a", NodeMeta{Runtime: "firecracker", Isolation: true, Templates: []string{"code-interpreter"}, TagNetworks: true}))
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 4000), later))

		n, _, err := ns.Get("node-a")
		if err != nil {
			t.Fatal(err)
		}
		if n.Runtime != "firecracker" || len(n.Templates) != 1 || !n.TagNetworks {
			t.Errorf("a heartbeat blanked the capability meta: runtime=%q templates=%v tag_networks=%v",
				n.Runtime, n.Templates, n.TagNetworks)
		}
		if !n.FirstSeen.Equal(first) {
			t.Errorf("FirstSeen = %v, want the ORIGINAL registration %v", n.FirstSeen, first)
		}
		if !n.LastSeen.Equal(later) || n.Cap.MemAvailMB != 4000 {
			t.Errorf("the heartbeat's own fields did not advance: %+v", n)
		}
	})
}

func TestCordonSurvivesHeartbeats(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now))

		found, err := ns.SetFlag("node-a", FlagCordoned, true)
		if err != nil || !found {
			t.Fatalf("SetFlag: found=%v err=%v", found, err)
		}
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 7000), now.Add(5*time.Second)))

		n, _, err := ns.Get("node-a")
		if err != nil {
			t.Fatal(err)
		}
		if !n.Cordoned {
			t.Error("a heartbeat cleared an operator's cordon — the node is back in scheduling")
		}
		if n.Cap.MemAvailMB != 7000 {
			t.Errorf("the cordon blocked the capacity update: %d", n.Cap.MemAvailMB)
		}

		if _, err := ns.SetFlag("node-a", FlagCordoned, false); err != nil {
			t.Fatal(err)
		}
		if n, _, _ := ns.Get("node-a"); n.Cordoned {
			t.Error("uncordon did not take effect")
		}
	})
}

func TestSetFlagOnUnknownNodeReportsNotFound(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		found, err := ns.SetFlag("ghost", FlagDraining, true)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Error("SetFlag reported success for an unknown node")
		}
	})
}

// A URL change on a live node is refused; on a stale one it is accepted and counted.
func TestURLTakeoverIsRefusedWhileTheNodeIsLive(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now))
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now.Add(5*time.Second)))
		if n, _, _ := ns.Get("node-a"); n.URLChanges != 0 {
			t.Errorf("a stable URL counted as churn: %d", n.URLChanges)
		}

		err := ns.Upsert(hb("node-a", "http://b:18080", "eu", 8000), now.Add(10*time.Second))
		if !errors.Is(err, ErrNodeURLTakeover) {
			t.Fatalf("url change on a live node: err = %v, want ErrNodeURLTakeover", err)
		}
		n, _, _ := ns.Get("node-a")
		if n.URL != "http://a:18080" {
			t.Errorf("url = %q after a refused takeover, want http://a:18080", n.URL)
		}
		if !n.LastSeen.Equal(now.Add(5 * time.Second)) {
			t.Errorf("LastSeen = %v after a REFUSED heartbeat, want %v — a refused attempt "+
				"that refreshes liveness holds its own window shut forever",
				n.LastSeen, now.Add(5*time.Second))
		}
		if n.URLChanges != 0 {
			t.Errorf("URLChanges = %d after a refused takeover, want 0", n.URLChanges)
		}

		stale := now.Add(5*time.Second + NodeTTL + time.Second)
		mustNil(t, ns.Upsert(hb("node-a", "http://b:18080", "eu", 8000), stale))
		n, _, _ = ns.Get("node-a")
		if n.URL != "http://b:18080" {
			t.Errorf("url = %q after the old registration aged out, want http://b:18080", n.URL)
		}
		if n.URLChanges != 1 {
			t.Errorf("URLChanges = %d, want 1 — the NODE_ID churn signal is lost", n.URLChanges)
		}
	})
}

func TestNodeStoreListAndDelete(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now))
		mustNil(t, ns.Upsert(hb("node-b", "http://b:18080", "us", 4000), now))

		all, err := ns.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Fatalf("List returned %d nodes, want 2", len(all))
		}

		found, err := ns.Delete("node-a")
		if err != nil || !found {
			t.Fatalf("Delete: found=%v err=%v", found, err)
		}
		if found, _ := ns.Delete("node-a"); found {
			t.Error("Delete reported success twice for one node")
		}
		if all, _ := ns.List(); len(all) != 1 {
			t.Fatalf("List after delete returned %d nodes, want 1", len(all))
		}
	})
}

func TestReservationsAccumulateAndExpire(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		exp := now.Add(ReservationTTL)

		mustNil(t, ns.Reserve("node-a", 1024, exp))
		mustNil(t, ns.Reserve("node-a", 2048, exp))
		mustNil(t, ns.Reserve("node-b", 512, exp))

		got, err := ns.ReservedMB(now)
		if err != nil {
			t.Fatal(err)
		}
		if got["node-a"] != 3072 || got["node-b"] != 512 {
			t.Fatalf("ReservedMB = %v, want node-a 3072 / node-b 512", got)
		}

		got, err = ns.ReservedMB(exp.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("expired reservations survived: %v", got)
		}
	})
}

func TestUnreserveDropsExactlyOneHold(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		exp := now.Add(ReservationTTL)
		mustNil(t, ns.Reserve("node-a", 1024, exp))
		mustNil(t, ns.Reserve("node-a", 1024, exp))
		mustNil(t, ns.Reserve("node-a", 2048, exp))

		mustNil(t, ns.Unreserve("node-a", 1024))
		got, _ := ns.ReservedMB(now)
		if got["node-a"] != 3072 {
			t.Fatalf("after releasing one 1024 hold, reserved = %d, want 3072", got["node-a"])
		}

		mustNil(t, ns.Unreserve("node-a", 9999))
		got, _ = ns.ReservedMB(now)
		if got["node-a"] != 3072 {
			t.Fatalf("a non-matching release changed the total: %d", got["node-a"])
		}
	})
}

func TestDeleteDropsTheNodesReservations(t *testing.T) {
	eachNodeStore(t, func(t *testing.T, ns NodeStore) {
		now := time.Unix(1_700_000_000, 0).UTC()
		mustNil(t, ns.Upsert(hb("node-a", "http://a:18080", "eu", 8000), now))
		mustNil(t, ns.Reserve("node-a", 1024, now.Add(ReservationTTL)))

		if _, err := ns.Delete("node-a"); err != nil {
			t.Fatal(err)
		}
		got, _ := ns.ReservedMB(now)
		if got["node-a"] != 0 {
			t.Fatalf("a removed node still holds %d MB reserved", got["node-a"])
		}
	})
}

package cluster

import (
	"errors"
	"testing"
	"time"
)

func pinBeat(id, url, spki string, ms int64) NodeUpsert {
	return NodeUpsert{ID: id, URL: url, Zone: "eu", Key: "k", TLSSPKI: spki, BeatMS: ms}
}

func TestReplayedHeartbeatIsRefused(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first := pinBeat("node-a", "http://a:18080", "", now.UnixMilli())

	if err := ns.Upsert(first, now); err != nil {
		t.Fatalf("first beat: %v", err)
	}
	// Positive control, so a store that refuses everything cannot pass.
	later := now.Add(5 * time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "", later.UnixMilli()), later); err != nil {
		t.Fatalf("a newer beat was refused: %v", err)
	}

	err := ns.Upsert(first, later.Add(time.Second))
	if !errors.Is(err, ErrStaleHeartbeat) {
		t.Fatalf("replaying the first heartbeat = %v, want ErrStaleHeartbeat", err)
	}

	n, ok, err := ns.Get("node-a")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if !n.LastSeen.Equal(later) {
		t.Errorf("LastSeen = %v, want %v — the refused beat still touched the record", n.LastSeen, later)
	}
}

func TestHeartbeatWithAnIdenticalTimestampIsRefused(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	b := pinBeat("node-a", "http://a:18080", "", now.UnixMilli())
	if err := ns.Upsert(b, now); err != nil {
		t.Fatal(err)
	}
	if err := ns.Upsert(b, now.Add(time.Second)); !errors.Is(err, ErrStaleHeartbeat) {
		t.Fatalf("re-sending the same timestamp = %v, want ErrStaleHeartbeat", err)
	}
}

func TestHeartbeatWithoutATimestampIsToleratedButDoesNotDisableTheRule(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := ns.Upsert(pinBeat("old", "http://o:18080", "", 0), now); err != nil {
		t.Fatalf("a pre-upgrade heartbeat was refused: %v", err)
	}
	if err := ns.Upsert(pinBeat("old", "http://o:18080", "", 0), now.Add(5*time.Second)); err != nil {
		t.Fatalf("a second pre-upgrade heartbeat was refused: %v", err)
	}
	up := now.Add(10 * time.Second)
	if err := ns.Upsert(pinBeat("old", "http://o:18080", "", up.UnixMilli()), up); err != nil {
		t.Fatalf("first timestamped beat refused: %v", err)
	}
	if err := ns.Upsert(pinBeat("old", "http://o:18080", "", up.UnixMilli()), up.Add(5*time.Second)); !errors.Is(err, ErrStaleHeartbeat) {
		t.Error("once a node sends timestamps, a replay must be refused")
	}
}

func TestHeartbeatOutsideTheSkewWindowIsRefused(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for name, ms := range map[string]int64{
		"far future": now.Add(2 * beatSkew).UnixMilli(),
		"far past":   now.Add(-2 * beatSkew).UnixMilli(),
	} {
		if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "", ms), now); !errors.Is(err, ErrStaleHeartbeat) {
			t.Errorf("%s clock = %v, want ErrStaleHeartbeat", name, err)
		}
	}
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "", now.Add(beatSkew/2).UnixMilli()), now); err != nil {
		t.Errorf("a beat inside the skew window was refused: %v", err)
	}
}

func TestTLSPinTakeoverIsRefusedWhileTheIncumbentIsLive(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-real", now.UnixMilli()), now); err != nil {
		t.Fatal(err)
	}

	t1 := now.Add(2 * time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-attacker", t1.UnixMilli()), t1); !errors.Is(err, ErrNodeTLSPinTakeover) {
		t.Fatalf("pin takeover = %v, want ErrNodeTLSPinTakeover", err)
	}
	n, _, _ := ns.Get("node-a")
	if n.TLSSPKI != "pin-real" {
		t.Fatalf("the stored pin changed to %q despite the refusal", n.TLSSPKI)
	}

	// Positive control: the real node keeps beating with its own pin.
	t2 := now.Add(4 * time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-real", t2.UnixMilli()), t2); err != nil {
		t.Fatalf("the real node's own beat was refused: %v", err)
	}

	// A genuine re-key is accepted once the old registration is stale.
	stale := t2.Add(NodeTTL + time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-rekeyed", stale.UnixMilli()), stale); err != nil {
		t.Fatalf("a re-key after NodeTTL was refused: %v", err)
	}
	n, _, _ = ns.Get("node-a")
	if n.TLSSPKI != "pin-rekeyed" {
		t.Errorf("pin after a legitimate re-key = %q, want pin-rekeyed", n.TLSSPKI)
	}
}

func TestAFirstPinIsAccepted(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "", now.UnixMilli()), now); err != nil {
		t.Fatal(err)
	}
	up := now.Add(5 * time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-new", up.UnixMilli()), up); err != nil {
		t.Fatalf("a node advertising its first pin was refused: %v", err)
	}
	n, _, _ := ns.Get("node-a")
	if n.TLSSPKI != "pin-new" {
		t.Errorf("pin = %q, want pin-new", n.TLSSPKI)
	}
}

func TestDroppingThePinDoesNotClearIt(t *testing.T) {
	ns := newMemNodeStore()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "pin-real", now.UnixMilli()), now); err != nil {
		t.Fatal(err)
	}
	next := now.Add(5 * time.Second)
	if err := ns.Upsert(pinBeat("node-a", "http://a:18080", "", next.UnixMilli()), next); err != nil {
		t.Fatalf("a beat with the pin stripped was refused outright: %v", err)
	}
	n, _, _ := ns.Get("node-a")
	if n.TLSSPKI != "pin-real" {
		t.Errorf("pin after a stripped-field beat = %q, want it retained as pin-real — "+
			"clearing it downgrades the hop to plaintext on an attacker's say-so", n.TLSSPKI)
	}
}

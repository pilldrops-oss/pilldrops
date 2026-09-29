package cluster

import (
	"encoding/json"
	"testing"
	"time"
)

// A create from a published capture must land only where one can be booted. On
// firecracker the capability is snapshots AND per-sandbox namespaces, so the
// runtime pin the create already carries promises neither: both nodes here are
// firecracker and only one can do it. The control is the same fleet with the
// flag off the request, which lands on the better fit that cannot.
func TestPickFiltersByNamedSnapshot(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	plain := runtimeNode("plain", "firecracker", 8000, 3000, 5000, now) // tighter fit, wins by default
	capable := runtimeNode("capable", "firecracker", 64000, 40000, 20000, now)
	capable.NamedSnapshot = true

	req := Request{MemMB: 1024, Runtime: "firecracker"}
	if got := Pick([]*Node{plain, capable}, req, now, ttl); got == nil || got.ID != "plain" {
		t.Fatalf("Pick(no snapshot) = %v, want plain (best fit, control)", got)
	}
	req.NamedSnapshot = true
	got := Pick([]*Node{plain, capable}, req, now, ttl)
	if got == nil || got.ID != "capable" {
		t.Fatalf("Pick(NamedSnapshot) = %v, want capable — the firecracker pin alone does not "+
			"promise FC_NETNS and FC_MEM_SNAPSHOTS, so plain takes the create and answers 501", got)
	}
	if got := Pick([]*Node{plain}, req, now, ttl); got != nil {
		t.Fatalf("Pick(NamedSnapshot) with no capable node = %v, want nil", got)
	}
}

// The capture-plane bits must survive the store round trip, or the scheduler
// filter and the fleet capability answer both read false on a node reporting true.
func TestTheStoredRecordKeepsTheCapturePlaneBits(t *testing.T) {
	var rec nodeRecord
	rec.setMeta(NodeMeta{Runtime: "firecracker", Checkpoint: true, NamedSnapshot: true, Standby: true})
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back nodeRecord
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if m := back.meta(); !m.Checkpoint || !m.NamedSnapshot || !m.Standby {
		t.Errorf("meta after a round trip = %+v, want all three set (stored as %s)", m, blob)
	}
	if n := back.node(nodeFlags{}); !n.Checkpoint || !n.NamedSnapshot || !n.Standby {
		t.Errorf("Node after a round trip = %+v, want all three set", n)
	}
}

// The registry's capability question is what tells a capability miss from a
// capacity one, and it must not count a cordoned or dead node.
func TestAnyWithCapability(t *testing.T) {
	now := time.Now()
	ttl := 30 * time.Second
	r := NewRegistry()
	for _, id := range []string{"n-plain", "n-capable"} {
		if err := r.Upsert(NodeUpsert{ID: id, URL: "http://" + id, Key: "k",
			Cap: Capacity{MemTotalMB: 64000, MemAvailMB: 40000}}, now); err != nil {
			t.Fatal(err)
		}
	}
	r.SetNodeMeta("n-plain", NodeMeta{Runtime: "firecracker"})
	if r.AnyWithCapability(CapNamedSnapshot, now, ttl) {
		t.Fatal("a fleet where nothing reports named_snapshot answered yes")
	}
	r.SetNodeMeta("n-capable", NodeMeta{Runtime: "firecracker", NamedSnapshot: true})
	if !r.AnyWithCapability(CapNamedSnapshot, now, ttl) {
		t.Fatal("a fleet with one capable node answered no")
	}
	r.SetCordon("n-capable", true)
	if r.AnyWithCapability(CapNamedSnapshot, now, ttl) {
		t.Error("a cordoned node still counted; nothing would take the create it enables")
	}
}

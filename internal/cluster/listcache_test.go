package cluster

import (
	"testing"
	"time"
)

// Two replicas over one store: a replica's own write is visible at once, the
// other's within listTTL, and editing a listed node never reaches the cache.
func TestSharedListCache(t *testing.T) {
	store := newMemNodeStore()
	a := NewRegistryShared(store, newMemRouteStore())
	b := NewRegistryShared(store, newMemRouteStore())
	a.listTTL, b.listTTL = 50*time.Millisecond, 50*time.Millisecond
	now := time.Now()

	if err := a.Upsert(NodeUpsert{ID: "n1", URL: "http://n1", Key: "k"}, now); err != nil {
		t.Fatal(err)
	}
	if len(b.Nodes()) != 1 {
		t.Fatal("b has never listed, so it must read the store")
	}
	if err := a.Upsert(NodeUpsert{ID: "n2", URL: "http://n2", Key: "k"}, now); err != nil {
		t.Fatal(err)
	}
	if len(a.Nodes()) != 2 {
		t.Fatal("a replica's own write must show at once")
	}
	if len(b.Nodes()) != 1 {
		t.Fatal("expected b's cached list within the TTL (this is the cache working)")
	}
	time.Sleep(60 * time.Millisecond)
	if len(b.Nodes()) != 2 {
		t.Fatal("b still missing a's node after the TTL")
	}

	b.Nodes()[0].Cap.MemAvailMB = -999
	if b.Nodes()[0].Cap.MemAvailMB == -999 {
		t.Fatal("an edit to a listed node leaked into the cache")
	}
}

// A move leaving the only node with the right CPU must hear "no CPU", not "no
// capacity": the node it is leaving does not count.
func TestAnyProvidingCPUIgnoresTheNodeBeingLeft(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	if err := r.Upsert(NodeUpsert{ID: "haswell", URL: "http://h", Key: "k"}, now); err != nil {
		t.Fatal(err)
	}
	r.SetNodeMeta("haswell", NodeMeta{CPUFeatures: []string{"avx2"}})
	req := Request{CPUFeatures: []string{"avx2"}}
	if !r.AnyProvidingCPU(req, now, NodeTTL) {
		t.Fatal("precondition: the haswell node provides avx2")
	}
	req.Avoid = []string{"haswell"}
	if r.AnyProvidingCPU(req, now, NodeTTL) {
		t.Fatal("the node a move is leaving was counted as a place it could go")
	}
}

func TestLocksAreExclusiveAndReleasable(t *testing.T) {
	l, _ := NewLocks("")
	rel, ok := l.TryLock("move:sb1", time.Minute)
	if !ok || !l.Held("move:sb1") {
		t.Fatal("first lock not taken")
	}
	if _, ok := l.TryLock("move:sb1", time.Minute); ok {
		t.Fatal("a second holder took a held lock")
	}
	rel()
	if l.Held("move:sb1") {
		t.Fatal("released lock still held")
	}
	if _, ok := l.TryLock("move:sb1", time.Millisecond); !ok {
		t.Fatal("could not retake a released lock")
	}
	time.Sleep(5 * time.Millisecond)
	if l.Held("move:sb1") {
		t.Fatal("an expired lock still reads as held")
	}
}

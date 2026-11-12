package cluster

import (
	"testing"
	"time"
)

type countingRoutes struct {
	RouteStore
	lookups int
}

func (c *countingRoutes) NodeFor(sandboxID string) (string, bool, error) {
	c.lookups++
	return c.RouteStore.NodeFor(sandboxID)
}

type countingNodes struct {
	NodeStore
	gets int
}

func (c *countingNodes) Get(id string) (*Node, bool, error) {
	c.gets++
	return c.NodeStore.Get(id)
}

// Against a shared store every lookup is a network round trip, and one sandbox
// request resolves its node more than once.
func TestNodeForReusesARecentAnswer(t *testing.T) {
	nodes := &countingNodes{NodeStore: newMemNodeStore()}
	routes := &countingRoutes{RouteStore: newMemRouteStore()}
	a := NewRegistryShared(nodes, routes)
	b := NewRegistryShared(nodes, routes)
	a.listTTL, b.listTTL = 50*time.Millisecond, 50*time.Millisecond
	now := time.Now()

	if err := a.Upsert(NodeUpsert{ID: "n1", URL: "http://n1", Key: "k"}, now); err != nil {
		t.Fatal(err)
	}
	if err := a.Upsert(NodeUpsert{ID: "n2", URL: "http://n2", Key: "k"}, now); err != nil {
		t.Fatal(err)
	}
	a.Place("sb1", "n1")

	for range 3 {
		if n, ok := b.NodeFor("sb1"); !ok || n.ID != "n1" {
			t.Fatalf("b.NodeFor = %v, %v", n, ok)
		}
	}
	if routes.lookups != 1 || nodes.gets != 0 {
		t.Fatalf("3 lookups within the TTL read the route %d times and the node %d times, want 1 and 0", routes.lookups, nodes.gets)
	}

	if _, ok := b.NodeFor("sb2"); ok {
		t.Fatal("sb2 is not placed yet")
	}
	a.Place("sb2", "n2")
	if n, ok := b.NodeFor("sb2"); !ok || n.ID != "n2" {
		t.Fatal("a miss was remembered: a sandbox another replica just placed did not resolve")
	}

	b.Unplace("sb1")
	if _, ok := b.NodeFor("sb1"); ok {
		t.Fatal("a replica's own unplace must show at once")
	}

	a.Place("sb2", "n1")
	time.Sleep(60 * time.Millisecond)
	if n, ok := b.NodeFor("sb2"); !ok || n.ID != "n1" {
		t.Fatal("b still routes to the old node after the TTL")
	}

	if !b.Remove("n1") {
		t.Fatal("remove n1")
	}
	if _, ok := b.NodeFor("sb2"); ok {
		t.Fatal("a sandbox on a removed node still resolved")
	}
}

func TestNodeForReadsThroughWithoutASharedStore(t *testing.T) {
	routes := &countingRoutes{RouteStore: newMemRouteStore()}
	r := &Registry{nodes: newMemNodeStore(), routes: routes}
	if err := r.Upsert(NodeUpsert{ID: "n1", URL: "http://n1", Key: "k"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Place("sb1", "n1")
	r.NodeFor("sb1")
	r.NodeFor("sb1")
	if routes.lookups != 2 {
		t.Fatalf("route read %d times, want 2", routes.lookups)
	}
}

package orchestrator

import "testing"

func TestPrivateConnSetCloseFor(t *testing.T) {
	var set privateConnSet
	closed := map[string]bool{}
	for _, c := range []*privateConn{{sandboxID: "a", target: "db"}, {sandboxID: "a", target: "api"}, {sandboxID: "b", target: "db"}} {
		name := c.sandboxID + "/" + c.target
		c.close = func() { closed[name] = true }
		set.add(c)
	}
	set.closeFor("a", "db")
	if !closed["a/db"] || closed["a/api"] || closed["b/db"] {
		t.Fatalf("closeFor(a, db) closed %v", closed)
	}
	set.closeFor("a", "")
	if !closed["a/api"] || closed["b/db"] {
		t.Fatalf("closeFor(a, all) closed %v", closed)
	}
	gone := &privateConn{sandboxID: "c", close: func() { t.Fatal("removed conn closed") }}
	set.add(gone)
	set.remove(gone)
	set.closeFor("c", "")
}

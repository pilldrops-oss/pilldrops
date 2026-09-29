package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestRemoveNodeCleansPlacements(t *testing.T) {
	r := cluster.NewRegistry()
	now := time.Now()
	upsert(r, "node-a", "http://x", "k", "", nil, "", cluster.Capacity{}, now)
	r.Place("sb1", "node-a")
	r.Place("sb2", "node-a")

	if !r.Remove("node-a") {
		t.Fatal("Remove(node-a) = false")
	}
	if _, ok := r.NodeFor("sb1"); ok {
		t.Error("sb1 still resolves to a removed node")
	}
	if got := r.PlacementsOn("node-a"); len(got) != 0 {
		t.Errorf("PlacementsOn after remove = %v, want none", got)
	}
}

func TestReapDeadNodes(t *testing.T) {
	r := cluster.NewRegistry()
	now := time.Now()
	upsert(r, "alive", "http://a", "k", "", nil, "", cluster.Capacity{}, now)
	upsert(r, "dead", "http://d", "k", "", nil, "", cluster.Capacity{}, now.Add(-5*time.Minute))
	r.Place("sb_alive", "alive")
	r.Place("sb_dead", "dead")

	reaped := r.ReapDeadNodes(now, 2*time.Minute)
	if len(reaped) != 1 || reaped[0] != "dead" {
		t.Fatalf("reaped = %v, want [dead]", reaped)
	}
	if _, ok := r.Node("alive"); !ok {
		t.Error("alive node was reaped")
	}
	if _, ok := r.NodeFor("sb_dead"); ok {
		t.Error("sb_dead placement not cleaned after reap")
	}
	if _, ok := r.NodeFor("sb_alive"); !ok {
		t.Error("sb_alive placement wrongly removed")
	}
}

func TestDrainReportsRemaining(t *testing.T) {
	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "node-a", "http://x", "kk", "", nil, "", cluster.Capacity{MemTotalMB: 8192, MemAvailMB: 8000}, now)
	s.reg.Place("sb1", "node-a")
	s.reg.Place("sb2", "node-a")

	req := httptest.NewRequest("POST", "/v1/nodes/node-a/drain", nil)
	req.SetPathValue("id", "node-a")
	// Calling the handler directly bypasses s.auth, which is what puts the
	// operator marker on the context. Draining is an operator action, so this
	// test has to present one; see TestChangingTheFleetNeedsTheOperatorKey for
	// the gate itself.
	req = req.WithContext(withOperator(req.Context()))
	w := httptest.NewRecorder()
	s.handleDrain(w, req)

	if w.Code != 200 {
		t.Fatalf("drain status = %d", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["sandboxes_remaining"].(float64) != 2 {
		t.Errorf("sandboxes_remaining = %v, want 2", out["sandboxes_remaining"])
	}
	if n := s.reg.PickAndReserve(cluster.Request{MemMB: 1024}, now, cluster.NodeTTL); n != nil {
		t.Errorf("PickAndReserve chose a draining node: %v", n.ID)
	}
	_ = http.StatusOK
}

// A drained node comes back into service on uncordon; nothing else clears a drain.
func TestUncordonReturnsADrainedNodeToService(t *testing.T) {
	s := NewServer("k", "t")
	now := time.Now()
	s.now = func() time.Time { return now }
	upsert(s.reg, "node-a", "http://a", "k", "", nil, "", cluster.Capacity{MemTotalMB: 8000, MemAvailMB: 8000}, now)
	call := func(verb string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/nodes/node-a/"+verb, nil)
		req.SetPathValue("id", "node-a")
		req = req.WithContext(withOperator(req.Context()))
		rw := httptest.NewRecorder()
		map[string]http.HandlerFunc{"drain": s.handleDrain, "uncordon": s.handleUncordon}[verb](rw, req)
		if rw.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", verb, rw.Code, rw.Body)
		}
	}
	call("drain")
	if n, _ := s.reg.Node("node-a"); n.Schedulable(now, cluster.NodeTTL) {
		t.Fatal("a draining node is still schedulable")
	}
	call("uncordon")
	if n, _ := s.reg.Node("node-a"); !n.Schedulable(now, cluster.NodeTTL) {
		t.Fatal("uncordon left the node drained, so it can never take work again")
	}
}

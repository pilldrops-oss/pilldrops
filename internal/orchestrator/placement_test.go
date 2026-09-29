package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestRebuildPlacementsFromNodes(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/sandboxes" {
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb1"},{"id":"sb2"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer node.Close()

	s := NewServer("api-key", "node-token")
	now := time.Now()
	upsert(s.reg, "node-a", node.URL, "node-key", "", nil, "", cluster.Capacity{}, now)

	if _, ok := s.reg.NodeFor("sb1"); ok {
		t.Fatal("precondition failed: placement should start empty")
	}

	if n := s.RebuildPlacements(now); n != 2 {
		t.Fatalf("rebuilt %d placements, want 2", n)
	}
	for _, id := range []string{"sb1", "sb2"} {
		nd, ok := s.reg.NodeFor(id)
		if !ok || nd.ID != "node-a" {
			t.Fatalf("NodeFor(%s) = %+v ok=%v, want node-a", id, nd, ok)
		}
	}
	// This node 404s /v1/admin/sandboxes, so this also pins the fallback for older nodes.
}

// The reconciler must enumerate sandboxes across all orgs, not just the operator's.
func TestRebuildPlacementsRecoversCustomerOrgs(t *testing.T) {
	var sawOrgOverride string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/admin/sandboxes":
			sawOrgOverride = r.Header.Get("X-Org-Override")
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-default"},{"id":"sb-cust-a"},{"id":"sb-cust-b"}]}`))
		case r.Method == "GET" && r.URL.Path == "/v1/sandboxes":
			// The operator-org view only.
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-default"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer node.Close()

	s := NewServer("api-key", "node-token")
	now := time.Now()
	upsert(s.reg, "node-a", node.URL, "node-key", "", nil, "", cluster.Capacity{}, now)

	if n := s.RebuildPlacements(now); n != 3 {
		t.Fatalf("rebuilt %d placements, want 3 (all orgs)", n)
	}
	for _, id := range []string{"sb-default", "sb-cust-a", "sb-cust-b"} {
		if nd, ok := s.reg.NodeFor(id); !ok || nd.ID != "node-a" {
			t.Fatalf("NodeFor(%s) ok=%v — a customer route the reconciler cannot rebuild is lost for good", id, ok)
		}
	}
	// The inventory is org-blind by route, not by impersonating an org.
	if sawOrgOverride != "" {
		t.Errorf("inventory sent X-Org-Override=%q, want none", sawOrgOverride)
	}
}

// Ten nodes that each take 300 ms to answer: serially that is 3 s, longer than
// a third of the reconcile interval. In parallel it is one node's latency.
func TestRebuildPlacementsCallsNodesInParallel(t *testing.T) {
	s := NewServer("api-key", "node-token")
	now := time.Now()
	for i := 0; i < 10; i++ {
		id := "sb" + string(rune('a'+i))
		node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"` + id + `"}]}`))
		}))
		t.Cleanup(node.Close)
		upsert(s.reg, "node-"+id, node.URL, "node-key", "", nil, "", cluster.Capacity{}, now)
	}

	start := time.Now()
	if n := s.RebuildPlacements(now); n != 10 {
		t.Fatalf("rebuilt %d placements, want 10", n)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("rebuild took %v for 10 nodes at 300 ms each: the fan-out is serial", d)
	}
}

package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// A node refuses another org's delete with 404; un-placing on that answer would
// let any tenant un-route another's running sandbox with one request.
func TestRejectedDestroyKeepsPlacement(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb-victim", "node-a")

	req := httptest.NewRequest("DELETE", "/v1/sandboxes/sb-victim", nil)
	req.SetPathValue("id", "sb-victim")
	req.Header.Set("X-Org-Override", "u:attacker")
	w := httptest.NewRecorder()
	s.handleDestroy(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("destroy relayed %d, want the node's 404", w.Code)
	}
	if nd, ok := s.reg.NodeFor("sb-victim"); !ok || nd.ID != "node-a" {
		t.Fatal("a rejected delete un-routed the sandbox: its owner can no longer reach a machine that is still running")
	}
}

func TestAcceptedDestroyUnplaces(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"destroyed"}`))
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb-mine", "node-a")

	req := httptest.NewRequest("DELETE", "/v1/sandboxes/sb-mine", nil)
	req.SetPathValue("id", "sb-mine")
	w := httptest.NewRecorder()
	s.handleDestroy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("destroy = %d, want 200", w.Code)
	}
	if _, ok := s.reg.NodeFor("sb-mine"); ok {
		t.Fatal("a successful destroy left the placement behind")
	}
}

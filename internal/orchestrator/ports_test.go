package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// Without an explicit orchestrator route the request 404s before reaching the node.
func TestSandboxPortsRelaysNodeResponse(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/sb1/ports" {
			t.Fatalf("node saw path %q, want /v1/sandboxes/sb1/ports", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ports":[{"port":3000,"listening":true},{"port":8000,"listening":false}]}`))
	}))
	t.Cleanup(node.Close)

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/ports", nil)
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handleSandboxPorts(w, req)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ports, _ := body["ports"].([]any)
	if len(ports) != 2 {
		t.Fatalf("ports = %v, want 2 entries", body["ports"])
	}
}

func TestSandboxPortsUnknownSandbox404s(t *testing.T) {
	s := NewServer("k", "t")
	req := httptest.NewRequest("GET", "/v1/sandboxes/nope/ports", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	s.handleSandboxPorts(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

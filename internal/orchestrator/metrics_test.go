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
func TestSandboxMetricsRelaysNodeResponse(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/sb1/metrics" {
			t.Fatalf("node saw path %q, want /v1/sandboxes/sb1/metrics", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"sb1","state":"running","vcpu":2,"mem_mib":1024,"cpu_pct":12.5,"mem_used_mib":300,"uptime_seconds":42}`))
	}))
	t.Cleanup(node.Close)

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/metrics", nil)
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handleSandboxMetrics(w, req)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != "running" {
		t.Fatalf("state = %v, want running", body["state"])
	}
	if body["cpu_pct"] != 12.5 {
		t.Fatalf("cpu_pct = %v, want 12.5", body["cpu_pct"])
	}
}

func TestSandboxMetricsUnknownSandbox404s(t *testing.T) {
	s := NewServer("k", "t")
	req := httptest.NewRequest("GET", "/v1/sandboxes/nope/metrics", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	s.handleSandboxMetrics(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestSandboxProxyForwardsBody(t *testing.T) {
	var got string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	req := httptest.NewRequest("POST", "/v1/sandboxes/sb1/exec", strings.NewReader(`{"cmd":"echo hi"}`))
	req.SetPathValue("id", "sb1")
	req.SetPathValue("rest", "exec")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleSandboxProxy(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("relay status = %d, want 200", w.Code)
	}
	if got != `{"cmd":"echo hi"}` {
		t.Fatalf("node received body %q, want the forwarded exec payload", got)
	}
}

// A node healthy by heartbeat but with its HTTP port down must be skipped for the next healthy node.
func TestProxyToNodeFailsOverDeadNode(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"vcpu_hours":1.5}`))
	}))
	defer alive.Close()

	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "node-dead", deadURL, "kk", "", nil, "", cluster.Capacity{}, now)
	upsert(s.reg, "node-alive", alive.URL, "kk", "", nil, "", cluster.Capacity{}, now)

	req := httptest.NewRequest("GET", "/v1/usage", nil)
	w := httptest.NewRecorder()
	s.proxyToNode(w, req, "GET", "/v1/usage")

	if w.Code != http.StatusOK {
		t.Fatalf("proxyToNode status = %d, want 200 (should fail over to the live node)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "vcpu_hours") {
		t.Fatalf("body = %q, want the live node's response", w.Body.String())
	}
}

// The bare GET /v1/sandboxes/{id} must route through the real mux, not just the {rest...} subpaths.
func TestSandboxGetByIDRoutes(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/sandboxes/sb-detail" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"sb-detail","status":"running"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb-detail", "node-a")

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb-detail", nil)
	req.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/sandboxes/{id} = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"sb-detail"`) {
		t.Fatalf("body = %s, want the node's sandbox detail", w.Body.String())
	}
}

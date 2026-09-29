package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The edge serves sandbox traffic and nothing that belongs to the API tier.
func TestEdgeRoutesServeOnlySandboxTraffic(t *testing.T) {
	h := NewServer("op-key", "node-token").EdgeRoutes()
	get := func(path string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer op-key")
		h.ServeHTTP(w, r)
		return w.Code
	}
	if get("/health") != http.StatusOK {
		t.Fatal("the edge has no health endpoint for its load balancer")
	}
	for _, p := range []string{"/v1/sandboxes", "/v1/nodes", "/dashboard/", "/install.sh", "/mcp"} {
		if c := get(p); c != http.StatusNotFound {
			t.Errorf("edge answered %s with %d; it must not serve the API tier", p, c)
		}
	}
	// Unknown sandbox: the preview handler answers, the mux does not 404 the path.
	if c := get("/preview/sb-nope/8000/"); c == http.StatusMethodNotAllowed || c == http.StatusOK {
		t.Errorf("preview path answered %d", c)
	}
}

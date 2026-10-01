package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Functions moved to the cloud product. The control plane must keep no handler
// of its own for them: each path reaches the node untouched and the node's 404
// (pinned by httpapi's TestMovedFeatureRoutesAreNotServed) comes back as is.
func TestMovedFeatureRoutesAreNotServedThroughTheControlPlane(t *testing.T) {
	var gotPath string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		http.NotFound(w, r)
	}))
	t.Cleanup(node.Close)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	if err := upsert(s.reg, "n1", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	cp := httptest.NewServer(s.Routes())
	t.Cleanup(cp.Close)
	tenant := orgKey(t, s, "u:tenant")

	for _, rq := range []struct{ method, path string }{
		{"GET", "/v1/functions"},
		{"POST", "/v1/functions"},
		{"GET", "/v1/functions/hello"},
		{"POST", "/v1/functions/hello/invoke"},
		{"GET", "/v1/fn/hello"},
	} {
		gotPath = ""
		req, _ := http.NewRequest(rq.method, cp.URL+rq.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+tenant)
		resp, err := cp.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rq.method, rq.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d through the control plane, want 404", rq.method, rq.path, resp.StatusCode)
		}
		if gotPath != rq.path {
			t.Errorf("%s %s reached the node as %q — the control plane answered it itself",
				rq.method, rq.path, gotPath)
		}
	}
}

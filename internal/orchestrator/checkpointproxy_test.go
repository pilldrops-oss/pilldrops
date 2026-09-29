package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The checkpoint verbs ride the {rest...} catch-all rather than routes of their
// own, so what has to hold is that it carries DELETE and a two-segment tail.
// Nothing else in the suite exercises either.
func TestCheckpointVerbsRideTheCatchAllProxy(t *testing.T) {
	for _, tc := range []struct{ method, rest, wantPath string }{
		{"POST", "checkpoints", "/v1/sandboxes/sb1/checkpoints"},
		{"GET", "checkpoints", "/v1/sandboxes/sb1/checkpoints"},
		{"DELETE", "checkpoints/c1", "/v1/sandboxes/sb1/checkpoints/c1"},
		{"POST", "checkpoints/c1/restore", "/v1/sandboxes/sb1/checkpoints/c1/restore"},
	} {
		t.Run(tc.method+" "+tc.rest, func(t *testing.T) {
			var gotMethod, gotPath string
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				w.WriteHeader(http.StatusOK)
			}))
			defer node.Close()

			s := NewServer("k", "t")
			upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
			s.reg.Place("sb1", "node-a")

			req := httptest.NewRequest(tc.method, "/v1/sandboxes/sb1/"+tc.rest, nil)
			req.SetPathValue("id", "sb1")
			req.SetPathValue("rest", tc.rest)
			w := httptest.NewRecorder()
			s.handleSandboxProxy(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("relay status = %d, want 200", w.Code)
			}
			if gotMethod != tc.method || gotPath != tc.wantPath {
				t.Fatalf("node saw %s %s, want %s %s", gotMethod, gotPath, tc.method, tc.wantPath)
			}
		})
	}
}

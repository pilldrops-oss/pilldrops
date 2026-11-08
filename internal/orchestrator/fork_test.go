package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// A fork made for an org without a key id, the portal's shape, is routed like
// a create: reading, exec'ing in and destroying the child all reach its node.
// The child of exactly this call was once a running sandbox no verb could find.
func TestAForkIsReachableAfterCreation(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sb-fork-1","status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sb-fork-1"}`))
	}))
	defer node.Close()

	s := NewServer("k", "t")
	if err := upsert(s.reg, "node-2", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb-parent", "node-2")
	h := s.Routes()

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/sandboxes/sb-parent/fork", `{"name":"fork-of-moved"}`},
		{"GET", "/v1/sandboxes/sb-fork-1", ""},
		{"POST", "/v1/sandboxes/sb-fork-1/exec", `{"cmd":"true"}`},
		{"DELETE", "/v1/sandboxes/sb-fork-1", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Org-Override", "u:rig-integration-test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code >= 300 {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		mu.Lock()
		reached := hits[tc.method+" "+tc.path]
		mu.Unlock()
		if reached != 1 {
			t.Errorf("%s %s reached the child's node %d times, want 1", tc.method, tc.path, reached)
		}
	}
}

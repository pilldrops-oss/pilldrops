package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A paused sandbox is woken for its move. When the move then failed it stayed
// running, and billed, on the node it never left.
func TestAFailedMoveLeavesAPausedSandboxPaused(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "move.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	var mu sync.Mutex
	var calls []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(node.Close)

	// No artifact store is configured, so the capture step fails.
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	rec := store.SandboxRecord{ID: "sb1", Template: "base", OrgID: "org-a", State: "paused", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1}
	res := s.moveSandbox(ctx, st, rec, &cluster.Node{ID: "n1", URL: node.URL, Key: "k"})
	if res.Error == "" {
		t.Fatal("the move succeeded; this test needs it to fail after the resume")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"POST /v1/sandboxes/sb1/resume", "POST /v1/sandboxes/sb1/pause"}
	if !slices.Equal(calls, want) {
		t.Errorf("node calls = %v, want %v", calls, want)
	}
}

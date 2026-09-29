package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// A list that silently drops an errored node looks complete to the client.
func TestListSurfacesUnreachableNodes(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb1"}]}`))
	}))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "good", good.URL, "kk", "", nil, "", cluster.Capacity{}, now)
	upsert(s.reg, "bad", bad.URL, "kk", "", nil, "", cluster.Capacity{}, now)

	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	w := httptest.NewRecorder()
	s.handleList(w, req)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["incomplete"] != true {
		t.Errorf("incomplete = %v, want true", out["incomplete"])
	}
	un, _ := out["unreachable_nodes"].([]any)
	if len(un) != 1 || un[0] != "bad" {
		t.Errorf("unreachable_nodes = %v, want [bad]", un)
	}
	if sbs, _ := out["sandboxes"].([]any); len(sbs) != 1 {
		t.Errorf("sandboxes = %v, want 1 (from the good node)", sbs)
	}
}

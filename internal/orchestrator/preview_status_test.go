package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The readiness vocabulary is the node's (internal/httpapi classifies it and
// tests it); this tier only has to hand it back, port query included.
func TestPreviewStatusRelaysTheNodesAnswer(t *testing.T) {
	var gotPath string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"paused","sandbox":"sb1","port":3000}`))
	}))
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/preview-status?port=3000", nil)
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handlePreviewStatus(w, req)

	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if gotPath != "/v1/sandboxes/sb1/preview-status?port=3000" {
		t.Fatalf("node saw %q; the port query must survive the hop", gotPath)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	// "paused" is a state this tier could not produce on its own, which is the point.
	if body["state"] != "paused" || body["port"] != float64(3000) {
		t.Fatalf("relayed body = %v", body)
	}
}

func TestPreviewStatusUnknownSandbox404s(t *testing.T) {
	s := NewServer("k", "t")
	req := httptest.NewRequest("GET", "/v1/sandboxes/nope/preview-status", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	s.handlePreviewStatus(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

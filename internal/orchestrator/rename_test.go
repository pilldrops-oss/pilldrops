package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func TestHandleSandboxRenameForwardsToNode(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"sb1","name":"renamed"}`))
	}))
	t.Cleanup(node.Close)

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, time.Now())
	s.reg.Place("sb1", "node-a")

	req := httptest.NewRequest("PATCH", "/v1/sandboxes/sb1", strings.NewReader(`{"name":"renamed"}`))
	req.SetPathValue("id", "sb1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleSandboxRename(w, req)

	if gotMethod != "PATCH" || gotPath != "/v1/sandboxes/sb1" {
		t.Fatalf("node saw method=%s path=%s, want PATCH /v1/sandboxes/sb1", gotMethod, gotPath)
	}
	if gotBody != `{"name":"renamed"}` {
		t.Fatalf("node saw body=%q, want the rename body intact", gotBody)
	}
	if w.Code != 200 {
		t.Fatalf("handler code = %d, want 200", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["name"] != "renamed" {
		t.Fatalf("relayed body name = %v, want renamed", out["name"])
	}
}

func TestHandleSandboxRenameUnknownSandbox404s(t *testing.T) {
	s := NewServer("k", "t")
	req := httptest.NewRequest("PATCH", "/v1/sandboxes/nope", strings.NewReader(`{"name":"x"}`))
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	s.handleSandboxRename(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

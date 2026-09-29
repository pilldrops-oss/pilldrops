package orchestrator

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// With a store wired and no node registered, list/usage/history answer locally
// in the node handlers' wire shapes.
func TestStoreFastPaths(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	now := time.Now().Unix()
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb-fast", OrgID: "org-a", Template: "base", State: "running",
		PublicPort: 8000, VCPU: 1, MemMiB: 1024, CreatedAt: now - 30, ExpiresAt: now + 870, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.RecordUsage(t.Context(), "sb-old", "org-a", store.UsageDelta{VCPUSeconds: 60, RAMGBSeconds: 60}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if err := st.RecordHistory(t.Context(), store.HistoryRecord{
		ID: "sb-old", OrgID: "org-a", Template: "base", ExitState: "destroyed",
		CreatedAt: now - 200, DestroyedAt: now - 140, DurationSecs: 60, VCPU: 1, MemMiB: 1024,
	}); err != nil {
		t.Fatalf("history: %v", err)
	}
	if err := st.SetSandboxAttribution(t.Context(), "sb-fast", "agent", "cli"); err != nil {
		t.Fatalf("attribution: %v", err)
	}

	s := NewServer("k", "t")
	s.SetStore(st)

	get := func(path string) map[string]any {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("X-Org-Override", "org-a")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d (%s)", path, w.Code, w.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("GET %s: bad json: %v", path, err)
		}
		return m
	}

	list := get("/v1/sandboxes")
	sbs, _ := list["sandboxes"].([]any)
	if len(sbs) != 1 {
		t.Fatalf("list = %d sandboxes, want 1: %v", len(sbs), list)
	}
	first, _ := sbs[0].(map[string]any)
	if first["id"] != "sb-fast" || first["state"] != "running" || first["public_port"] != float64(8000) {
		t.Fatalf("list row shape wrong: %v", first)
	}

	usage := get("/v1/usage")
	// total_starts counts metered sandboxes only; sb-fast has no usage deltas yet.
	if usage["total_starts"] != float64(1) {
		t.Fatalf("total_starts = %v, want 1", usage["total_starts"])
	}
	if usage["active"] != float64(1) {
		t.Fatalf("active = %v, want 1", usage["active"])
	}
	if _, ok := usage["points"].([]any); !ok {
		t.Fatalf("usage missing points: %v", usage)
	}
	byClient, ok := usage["by_client"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing by_client (or wrong shape): %v", usage)
	}
	if v, _ := byClient["cli"].(float64); v != 1 {
		t.Fatalf("by_client.cli = %v, want 1: %v", byClient["cli"], byClient)
	}

	hist := get("/v1/history")
	recs, _ := hist["history"].([]any)
	if len(recs) != 1 {
		t.Fatalf("history = %d, want 1", len(recs))
	}
}

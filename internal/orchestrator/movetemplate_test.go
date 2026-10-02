package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A moved sandbox listed as snap-37a8eec1-move-<id>: its boot template, the
// move capture, rather than the template its owner created it from.
func TestAMovedSandboxIsListedUnderItsOriginalTemplate(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "move.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(ctx, "a", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	// Moved three times: each capture names the previous move's template. The
	// first predates per-move names and still answers under the bare name.
	names := []string{store.MoveSnapshotName("sb1"), store.MoveSnapshotName("sb1") + "-0123abcd", store.MoveSnapshotName("sb1") + "-4567cdef"}
	captured := "python-3.11"
	for _, name := range names {
		snap := store.Snapshot{OrgID: "org-a", Name: name, Runtime: "firecracker", SourceSandbox: "sb1", CreatedAt: 1}
		if err := st.SaveSnapshot(ctx, &snap); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateSnapshotCapture(ctx, snap.ID, captured, 1, 512, nil); err != nil {
			t.Fatal(err)
		}
		captured = store.SnapshotTemplateID("org-a", name)
	}
	moveTemplate := captured
	for _, rec := range []store.SandboxRecord{
		{ID: "sb1", Template: moveTemplate, OrgID: "org-a", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1},
		{ID: "sb2", Template: "base", OrgID: "org-a", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1},
	} {
		if err := st.SaveSandbox(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	orch := httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)

	req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Sandboxes []storeSandboxJSON `json:"sandboxes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sb := range page.Sandboxes {
		got[sb.ID] = sb.Template
	}
	if got["sb1"] != "python-3.11" || got["sb2"] != "base" {
		t.Errorf("listed templates = %v, want sb1 python-3.11 and sb2 base", got)
	}
}

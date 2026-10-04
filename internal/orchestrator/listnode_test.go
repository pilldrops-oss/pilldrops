package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The cluster page groups sandboxes by node, and the list it reads carried
// none. Tenants still get no node id: /v1/nodes is operator-only for that reason.
func TestTheListNamesEachSandboxNodeForOperatorsOnly(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "list.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, member, err := st.CreateKey(t.Context(), "m", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	_, op, err := st.CreateKeyWithRole(t.Context(), "op", "org-a", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", Template: "base", OrgID: "org-a", NodeID: "n1", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	orch := httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)

	nodeOf := func(key string) any {
		req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var page struct {
			Sandboxes []map[string]any `json:"sandboxes"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&page) != nil || len(page.Sandboxes) != 1 {
			t.Fatalf("list = %d %v", resp.StatusCode, page)
		}
		return page.Sandboxes[0]["node"]
	}
	if got := nodeOf(op); got != "n1" {
		t.Errorf("operator list: node = %v, want n1", got)
	}
	if got := nodeOf(member); got != nil {
		t.Errorf("member list: node = %v, want it absent", got)
	}
}

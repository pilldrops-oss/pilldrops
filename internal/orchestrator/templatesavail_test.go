package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// S1 made /v1/nodes operator-only, so a tenant could no longer tell which
// templates any node will boot. The catalogue now says so per entry, by the
// scheduler's rule, without naming a node.
func TestTemplatesSayWhetherSomeNodeCanBootThem(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"base"},{"id":"desktop"},{"id":"rust"}]`))
	}))
	t.Cleanup(node.Close)
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "tpl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, member, err := st.CreateKey(t.Context(), "m", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	capy := cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}
	// n2 is cordoned, so its desktop does not count.
	for id, tpl := range map[string]string{"n1": "base", "n2": "desktop"} {
		if err := s.reg.Upsert(cluster.NodeUpsert{ID: id, URL: node.URL, Key: "k-" + id, Cap: capy}, s.now()); err != nil {
			t.Fatal(err)
		}
		s.reg.SetNodeMeta(id, cluster.NodeMeta{Runtime: "docker", Templates: []string{tpl}})
	}
	s.reg.SetCordon("n2", true)
	orch := httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)

	req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/templates", nil)
	req.Header.Set("Authorization", "Bearer "+member)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cat []map[string]any
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&cat) != nil {
		t.Fatalf("templates = %d", resp.StatusCode)
	}
	want := map[string]bool{"base": true, "desktop": false, "rust": false}
	for _, tpl := range cat {
		id, _ := tpl["id"].(string)
		if tpl["available"] != want[id] {
			t.Errorf("%s: available = %v, want %v", id, tpl["available"], want[id])
		}
		for k := range tpl {
			if strings.Contains(k, "node") {
				t.Errorf("%s carries %q, which names a node", id, k)
			}
		}
	}
	if len(cat) != 3 {
		t.Errorf("got %d templates, want the node's 3", len(cat))
	}
}

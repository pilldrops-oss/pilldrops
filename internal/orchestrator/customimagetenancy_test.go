package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A live create goes through this tier, which the node treats as the operator,
// so a check on the node alone would never fire for a tenant.
func TestATenantCannotBootAnotherOrgsCustomImageThroughTheControlPlane(t *testing.T) {
	var creates atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			creates.Add(1)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sb-1"}`))
	}))
	t.Cleanup(node.Close)
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "tpl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "m", "org-b")
	if err != nil {
		t.Fatal(err)
	}
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: node.URL, Key: "k-n1",
		Cap: cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}}, s.now()); err != nil {
		t.Fatal(err)
	}
	orch := httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)

	for _, body := range []string{
		`{"template":"sf-org-a-tool"}`,
		`{"template":"base","image":"sf-org-a-tool:latest"}`,
		`{"template":"base","image":"docker.io/library/sf-org-a-tool:latest"}`,
	} {
		req, _ := http.NewRequest(http.MethodPost, orch.URL+"/v1/sandboxes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, resp.StatusCode)
		}
	}
	if n := creates.Load(); n != 0 {
		t.Errorf("%d foreign creates reached a node", n)
	}
}

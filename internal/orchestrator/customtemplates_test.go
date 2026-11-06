package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// An org's built image is in its own catalogue and nobody else's.
func TestOnlyTheBuildingOrgListsACustomImage(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/images" {
			_, _ = w.Write([]byte(`{"images":[{"id":"base"}]}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"base"}]`))
	}))
	t.Cleanup(node.Close)
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "tpl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	build := func(id, template, org string, ready bool) {
		t.Helper()
		if err := st.BeginTemplateBuild(t.Context(), store.TemplateBuild{BuildID: id, TemplateID: template, OrgID: org, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if !ready {
			return
		}
		if err := st.MarkTemplateBuildReady(t.Context(), store.TemplateBuild{
			BuildID: id, RootfsSize: 1 << 20, RootfsSHA256: strings.Repeat("a", 64), TotalSize: 5 << 20, ReadyAt: 9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	build("b1", "sf-org-a-tool", "org-a", true)
	build("b2", "sf-org-b-other", "org-b", true)
	build("b3", "sf-org-a-unfinished", "org-a", false)
	build("b4", "snap-capture", "org-a", true)
	_, memberA, err := st.CreateKey(t.Context(), "a", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	_, memberB, err := st.CreateKey(t.Context(), "b", "org-b")
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

	list := func(path, key string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, orch.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if !json.Valid(raw) || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", path, resp.StatusCode, raw)
		}
		return string(raw)
	}
	for _, path := range []string{"/v1/templates", "/v1/images"} {
		a, b := list(path, memberA), list(path, memberB)
		if !strings.Contains(a, `"sf-org-a-tool"`) || !strings.Contains(a, `"base"`) {
			t.Errorf("%s for the builder lacks its image: %s", path, a)
		}
		if strings.Contains(a, "sf-org-b-other") || strings.Contains(b, "sf-org-a-tool") {
			t.Errorf("%s leaked another org's image:\n%s\n%s", path, a, b)
		}
		if strings.Contains(a, "unfinished") || strings.Contains(a, "snap-capture") {
			t.Errorf("%s lists a build it must not: %s", path, a)
		}
	}
}

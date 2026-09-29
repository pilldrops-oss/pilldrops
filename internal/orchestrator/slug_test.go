package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// slugFleet is an orchestrator with a store holding org-a's "myapp" (sb1,
// placed on a recording fake node) and keys for two orgs.
func slugFleet(t *testing.T) (orch *httptest.Server, s *Server, keyA, keyB string, sawPath *string) {
	t.Helper()
	var path string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"id":"sb1","template":"base","status":"running"}`))
	}))
	t.Cleanup(node.Close)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "slug.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, keyA, err = st.CreateKey(t.Context(), "a", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err = st.CreateKey(t.Context(), "b", "org-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []store.SandboxRecord{
		{ID: "sb1", Template: "base", Slug: "myapp", OrgID: "org-a", State: "running", PublicPort: 3000, CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1},
		// A slug that spells another sandbox's *id*: the id must win for the org that owns it.
		{ID: "web", Template: "base", OrgID: "org-a", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1},
		{ID: "sb2", Template: "base", Slug: "web", OrgID: "org-a", State: "running", CreatedAt: 1, ExpiresAt: 2, UpdatedAt: 1},
	} {
		if err := st.SaveSandbox(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}

	s = fleetServer("apikey", "nodetoken")
	s.SetStore(st)
	s.sandboxDomain = ingressDomain
	orch = httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)
	registerNode(t, orch, `{"id":"n1","url":"`+node.URL+`","zone":"z1",`+
		`"capacity":{"cpu_cores":4,"mem_total_mb":8000,"mem_available_mb":4000}}`)
	s.reg.Place("sb1", "n1")
	s.reg.Place("sb2", "n1")
	s.reg.Place("web", "n1")
	return orch, s, keyA, keyB, &path
}

func getAs(t *testing.T, orch *httptest.Server, key, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, orch.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A path segment that is the caller's slug is rewritten to the id before the
// forward; another org's slug is a 404 indistinguishable from a bad id.
func TestSlugResolvesToTheOwnersSandboxOnly(t *testing.T) {
	orch, _, keyA, keyB, saw := slugFleet(t)

	if code := getAs(t, orch, keyA, "/v1/sandboxes/myapp"); code != http.StatusOK {
		t.Fatalf("owner GET by slug = %d, want 200", code)
	}
	if *saw != "/v1/sandboxes/sb1" {
		t.Fatalf("node saw %q, want /v1/sandboxes/sb1 (the node only ever sees ids)", *saw)
	}
	*saw = ""
	if code := getAs(t, orch, keyA, "/v1/sandboxes/myapp/ports"); code != http.StatusOK || *saw != "/v1/sandboxes/sb1/ports" {
		t.Fatalf("sub-route by slug = %d, node saw %q", code, *saw)
	}

	*saw = ""
	if code := getAs(t, orch, keyB, "/v1/sandboxes/myapp"); code != http.StatusNotFound {
		t.Fatalf("other org GET by slug = %d, want 404", code)
	}
	if *saw != "" {
		t.Fatalf("another org's slug reached the node as %q — cross-org resolution", *saw)
	}
	if code := getAs(t, orch, keyB, "/v1/sandboxes/no-such-thing"); code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404 (must match the cross-org answer)", code)
	}

	// A placed id that also spells a slug is taken as the id.
	*saw = ""
	if code := getAs(t, orch, keyA, "/v1/sandboxes/web"); code != http.StatusOK || *saw != "/v1/sandboxes/web" {
		t.Fatalf("id-shaped slug: code %d, node saw %q, want the id /v1/sandboxes/web", code, *saw)
	}
}

// <slug>.<domain> is rewritten into the holder's /preview/<id>/<public port>/ path.
func TestSlugHostRewritesIntoThePreviewPath(t *testing.T) {
	_, s, _, _, _ := slugFleet(t)
	sink := &ingressSink{}
	h := s.hostIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.hits++
		sink.path = r.URL.Path
		sink.rawQuery = r.URL.RawQuery
	}))

	h.ServeHTTP(httptest.NewRecorder(), ingressReq("myapp."+ingressDomain, "/app/index.html?token=t"))
	if want := "/preview/sb1/3000/app/index.html"; sink.path != want {
		t.Fatalf("slug host rewritten to %q, want %q (public port 3000)", sink.path, want)
	}
	if sink.rawQuery != "token=t" {
		t.Fatalf("query %q lost: the preview token rides in it", sink.rawQuery)
	}

	// Unknown slug and www fall through untouched; the id form still works.
	for _, host := range []string{"nope." + ingressDomain, "www." + ingressDomain} {
		sink.path = ""
		h.ServeHTTP(httptest.NewRecorder(), ingressReq(host, "/site"))
		if sink.path != "/site" {
			t.Errorf("%s reached next as %q, want /site untouched", host, sink.path)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), ingressReq("8080-sb1."+ingressDomain, "/x"))
	if sink.path != "/preview/sb1/8080/x" {
		t.Fatalf("id form rewritten to %q", sink.path)
	}
}

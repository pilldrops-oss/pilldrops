package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func newProjectServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, st
}

func do(t *testing.T, srv *httptest.Server, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestProjectsAreServedByTheControlPlane(t *testing.T) {
	srv, _ := newProjectServer(t)

	code, out := do(t, srv, "POST", "/v1/projects", "fleet-key", `{"name":"my app"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d (%v) — projects must not 404 on this tier", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", out)
	}

	if code, _ := do(t, srv, "GET", "/v1/projects/"+id, "fleet-key", ""); code != http.StatusOK {
		t.Errorf("get = %d, want 200", code)
	}
	code, out = do(t, srv, "GET", "/v1/projects", "fleet-key", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if list, _ := out["projects"].([]any); len(list) != 1 {
		t.Errorf("list returned %d projects, want 1", len(list))
	}
}

func TestProjectNameIsRequired(t *testing.T) {
	srv, _ := newProjectServer(t)
	if code, _ := do(t, srv, "POST", "/v1/projects", "fleet-key", `{"name":"  "}`); code != http.StatusBadRequest {
		t.Errorf("create with a blank name = %d, want 400", code)
	}
}

func TestProjectsAreScopedToTheAuthenticatedOrg(t *testing.T) {
	srv, st := newProjectServer(t)
	_, aKey, err := st.CreateKey(t.Context(), "a", "u:alice")
	if err != nil {
		t.Fatal(err)
	}
	_, bKey, err := st.CreateKey(t.Context(), "b", "u:bob")
	if err != nil {
		t.Fatal(err)
	}

	_, out := do(t, srv, "POST", "/v1/projects", aKey, `{"name":"alice private"}`)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("alice could not create: %v", out)
	}

	if code, _ := do(t, srv, "GET", "/v1/projects/"+id, bKey, ""); code != http.StatusNotFound {
		t.Errorf("bob read alice's project by id: %d, want 404", code)
	}
	_, out = do(t, srv, "GET", "/v1/projects", bKey, "")
	if list, _ := out["projects"].([]any); len(list) != 0 {
		t.Errorf("bob's list contains %d of alice's projects", len(list))
	}
	// Archiving someone else's is a 404, not a silent success.
	if code, _ := do(t, srv, "DELETE", "/v1/projects/"+id, bKey, ""); code != http.StatusNotFound {
		t.Errorf("bob archived alice's project: %d, want 404", code)
	}
	if code, _ := do(t, srv, "GET", "/v1/projects/"+id, aKey, ""); code != http.StatusOK {
		t.Errorf("alice's project is gone after bob's delete attempt: %d", code)
	}
}

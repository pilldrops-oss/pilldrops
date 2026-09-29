package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func projectRouter(t *testing.T, owner string) (chi.Router, *store.Store) {
	t.Helper()
	st := openTempStore(t)
	r := chi.NewRouter()
	registerProjectRoutes(r, st, func(*http.Request) string { return owner })
	return r, st
}

func do(t *testing.T, r chi.Router, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Create, list, fetch and run, the whole loop.
func TestProjectLifecycle(t *testing.T) {
	r, _ := projectRouter(t, "u:alice")

	w := do(t, r, "POST", "/v1/projects", `{"name":"checkout-api","plan":"{\"services\":[]}"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("create returned no id: %s", w.Body.String())
	}
	if created.Name != "checkout-api" {
		t.Fatalf("name = %q", created.Name)
	}

	w = do(t, r, "GET", "/v1/projects", "")
	var list struct {
		Projects []store.Project `json:"projects"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Projects) != 1 {
		t.Fatalf("list n=%d: %s", len(list.Projects), w.Body.String())
	}

	w = do(t, r, "POST", "/v1/projects/"+created.ID+"/runs",
		`{"plan_snapshot":"{\"v\":1}","services":[{"service_id":"api","sandbox_id":"sb-1"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}

	w = do(t, r, "GET", "/v1/projects/"+created.ID+"/runs", "")
	var runs struct {
		Runs []store.Run `json:"runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &runs)
	if len(runs.Runs) != 1 || runs.Runs[0].Services[0].SandboxID != "sb-1" {
		t.Fatalf("runs: %s", w.Body.String())
	}
}

// One customer must never reach another's project.
func TestProjectsAreOwnerScopedOverHTTP(t *testing.T) {
	rAlice, st := projectRouter(t, "u:alice")
	w := do(t, rAlice, "POST", "/v1/projects", `{"name":"alice-only"}`)
	var created store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	rBob := chi.NewRouter()
	registerProjectRoutes(rBob, st, func(*http.Request) string { return "u:bob" })

	if w := do(t, rBob, "GET", "/v1/projects", ""); strings.Contains(w.Body.String(), "alice-only") {
		t.Fatalf("bob saw alice's project: %s", w.Body.String())
	}
	if w := do(t, rBob, "GET", "/v1/projects/"+created.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("bob fetched alice's project: %d", w.Code)
	}
	if w := do(t, rBob, "POST", "/v1/projects/"+created.ID+"/runs", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("bob ran alice's project: %d", w.Code)
	}
}

func TestCreateProjectRejectsAnEmptyName(t *testing.T) {
	r, _ := projectRouter(t, "u:alice")
	// A project is a thing you return to; an unnamed one cannot be found again.
	if w := do(t, r, "POST", "/v1/projects", `{"name":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty name accepted: %d", w.Code)
	}
	if w := do(t, r, "POST", "/v1/projects", `not json`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json accepted: %d", w.Code)
	}
}

// Renaming and re-planning must not touch run history.
func TestUpdateProjectLeavesRunsAlone(t *testing.T) {
	r, _ := projectRouter(t, "u:alice")
	w := do(t, r, "POST", "/v1/projects", `{"name":"x","plan":"{\"v\":1}"}`)
	var p store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	_ = do(t, r, "POST", "/v1/projects/"+p.ID+"/runs", `{"plan_snapshot":"{\"v\":1}"}`)

	if w := do(t, r, "PATCH", "/v1/projects/"+p.ID, `{"name":"renamed","plan":"{\"v\":2}"}`); w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}

	w = do(t, r, "GET", "/v1/projects/"+p.ID+"/runs", "")
	var runs struct {
		Runs []store.Run `json:"runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &runs)
	if len(runs.Runs) != 1 || runs.Runs[0].PlanSnapshot != `{"v":1}` {
		t.Fatalf("run snapshot rewritten by a project edit: %s", w.Body.String())
	}
}

func TestArchiveHidesButKeeps(t *testing.T) {
	r, _ := projectRouter(t, "u:alice")
	w := do(t, r, "POST", "/v1/projects", `{"name":"x"}`)
	var p store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &p)

	if w := do(t, r, "DELETE", "/v1/projects/"+p.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("archive: %d", w.Code)
	}
	w = do(t, r, "GET", "/v1/projects", "")
	if strings.Contains(w.Body.String(), p.ID) {
		t.Fatalf("archived project still listed")
	}
	// Still fetchable by id: hidden from the list, not destroyed.
	if w := do(t, r, "GET", "/v1/projects/"+p.ID, ""); w.Code != http.StatusOK {
		t.Fatalf("archived project was deleted: %d", w.Code)
	}
}

// Without a store the routes must not mount rather than nil-panic.
func TestProjectRoutesAreOptional(t *testing.T) {
	r := chi.NewRouter()
	registerProjectRoutes(r, nil, func(*http.Request) string { return "u:alice" })
	if w := do(t, r, "GET", "/v1/projects", ""); w.Code != http.StatusNotFound {
		t.Fatalf("routes mounted without a store: %d", w.Code)
	}
}

// openTempStore opens a fresh SQLite store; the store package's openTemp is unexported.
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

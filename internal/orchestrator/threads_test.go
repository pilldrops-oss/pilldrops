package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func threadServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, keyA, err := st.CreateKey(t.Context(), "acme", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err := st.CreateKey(t.Context(), "other", "org-b")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer("fleet-key", "node-token")
	s.SetStore(st)
	return s, keyA, keyB
}

func threadReq(t *testing.T, s *Server, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func TestThreadRoundTripThroughTheControlPlane(t *testing.T) {
	s, key, _ := threadServer(t)

	w := threadReq(t, s, http.MethodPost, "/v1/threads", key, "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	var created store.Thread
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created thread has no id")
	}
	if created.Title != untitledThread {
		t.Errorf("title = %q, want the untitled fallback %q — a blank row reads as a bug",
			created.Title, untitledThread)
	}

	w = threadReq(t, s, http.MethodPatch, "/v1/threads/"+created.ID, key, `{"turns":"[1,2,3]"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	var patched store.Thread
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if patched.Turns != "[1,2,3]" {
		t.Errorf("turns = %q, want the saved transcript", patched.Turns)
	}
	if patched.Title != untitledThread {
		t.Errorf("title changed to %q on a turns-only patch — the pointer fields exist "+
			"precisely so 'append a turn' cannot clear a rename", patched.Title)
	}

	w = threadReq(t, s, http.MethodGet, "/v1/threads", key, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ID) {
		t.Errorf("list = %d, body %q — the new thread is missing", w.Code, w.Body.String())
	}

	if w := threadReq(t, s, http.MethodDelete, "/v1/threads/"+created.ID, key, ""); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", w.Code)
	}
	if w := threadReq(t, s, http.MethodGet, "/v1/threads/"+created.ID, key, ""); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404 — a thread DELETE really deletes", w.Code)
	}
}

func TestThreadsAreScopedToTheAuthenticatedOrg(t *testing.T) {
	s, keyA, keyB := threadServer(t)

	w := threadReq(t, s, http.MethodPost, "/v1/threads", keyA, `{"title":"acme private"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create as A = %d", w.Code)
	}
	var mine store.Thread
	_ = json.Unmarshal(w.Body.Bytes(), &mine)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/threads/" + mine.ID},
		{http.MethodPatch, "/v1/threads/" + mine.ID},
		{http.MethodDelete, "/v1/threads/" + mine.ID},
	} {
		t.Run(tc.method, func(t *testing.T) {
			body := ""
			if tc.method == http.MethodPatch {
				body = `{"title":"stolen"}`
			}
			if w := threadReq(t, s, tc.method, tc.path, keyB, body); w.Code != http.StatusNotFound {
				t.Errorf("org-b %s another org's thread = %d, want 404", tc.method, w.Code)
			}
		})
	}

	w = threadReq(t, s, http.MethodGet, "/v1/threads", keyB, "")
	if strings.Contains(w.Body.String(), "acme private") {
		t.Errorf("org-b's list contains org-a's thread: %s", w.Body.String())
	}
}

func TestThreadRejectsAnOversizedTranscript(t *testing.T) {
	s, key, _ := threadServer(t)
	big := `{"turns":"` + strings.Repeat("x", maxTurnsBytes+1) + `"}`

	if w := threadReq(t, s, http.MethodPost, "/v1/threads", key, big); w.Code != http.StatusBadRequest {
		t.Errorf("oversized create = %d, want 400", w.Code)
	}
}

func TestThreadRoutesAbsentWithoutAStore(t *testing.T) {
	s := NewServer("fleet-key", "node-token") // no SetStore
	w := threadReq(t, s, http.MethodGet, "/v1/threads", "fleet-key", "")
	if w.Code == http.StatusInternalServerError {
		t.Errorf("no-store list = 500 — the routes were mounted without a store and panicked")
	}
}

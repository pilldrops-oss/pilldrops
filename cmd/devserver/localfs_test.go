package main

// These tests drive the real file/filesystem handlers through chi: paths must
// stay inside the sandbox workspace and only the owner may reach it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// localFSRouter mounts the file handlers with ownerID pinned to a literal owner;
// production resolves every unauthenticated caller to "anon", so a cross-owner
// test needs two routers.
func localFSRouter(mgr *DevManager, owner string) chi.Router {
	r := chi.NewRouter()
	ownerID := func(*http.Request) string { return owner }
	registerFilesRoutes(r, mgr, ownerID)
	registerFilesystemRoutes(r, mgr, ownerID)
	return r
}

// newTestDevManager builds a DevManager with one sandbox, bypassing Create
// (dials a real envd) and Destroy (would delete t.TempDir()).
func newTestDevManager(t *testing.T, id, userID string) (*DevManager, string) {
	t.Helper()
	workDir := t.TempDir()
	mgr := &DevManager{
		sandboxes: map[string]*DevSandbox{
			id: {ID: id, UserID: userID, WorkDir: workDir},
		},
	}
	return mgr, workDir
}

func TestLocalFS_TraversalRejected(t *testing.T) {
	mgr, workDir := newTestDevManager(t, "sb-1", "u:alice")
	r := localFSRouter(mgr, "u:alice")

	// A write escaping toward /etc/passwd must be rejected before any OS call.
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/files?path=../../../../etc/passwd", strings.NewReader("pwned"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal write to /etc/passwd: got %d, want 400: %s", w.Code, w.Body.String())
	}

	// A second target inside t.TempDir(), so the "nothing escaped" check does
	// not depend on host permissions.
	escapeTarget := filepath.Clean(filepath.Join(workDir, "..", "..", "escaped.txt"))
	req2 := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/files?path=../../escaped.txt", strings.NewReader("pwned"))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("traversal write (controlled target): got %d, want 400: %s", w2.Code, w2.Body.String())
	}
	if _, err := os.Stat(escapeTarget); !os.IsNotExist(err) {
		t.Fatalf("traversal escaped workDir: %s exists (err=%v)", escapeTarget, err)
	}

	// Read must reject the same way.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/files?path=../../../../etc/passwd", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("traversal read: got %d, want 400: %s", w3.Code, w3.Body.String())
	}

	// The filesystem CRUD family must reject the same way.
	cases := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/filesystem/list?path=../../", nil),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/mkdir", strings.NewReader(`{"path":"../../escaped-dir"}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/move", strings.NewReader(`{"source":"a.txt","destination":"../../escaped-move.txt"}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/remove", strings.NewReader(`{"path":"../../escaped-remove.txt"}`)),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/filesystem/stat?path=../../escaped-stat.txt", nil),
	}
	for _, req := range cases {
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: got %d, want 400: %s", req.Method, req.URL.String(), w.Code, w.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Clean(filepath.Join(workDir, "..", "..", "escaped-dir"))); !os.IsNotExist(err) {
		t.Fatalf("mkdir traversal escaped workDir")
	}
}

func TestLocalFS_RemoveRootRefused(t *testing.T) {
	mgr, workDir := newTestDevManager(t, "sb-1", "u:alice")
	r := localFSRouter(mgr, "u:alice")

	for _, path := range []string{"", ".", "/"} {
		body := fmt.Sprintf(`{"path":%q}`, path)
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/remove", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("remove root (path=%q): got %d, want 400: %s", path, w.Code, w.Body.String())
		}
		if _, err := os.Stat(workDir); err != nil {
			t.Fatalf("remove root (path=%q) wiped the workspace: %v", path, err)
		}
	}
}

// TestLocalFS_CrossOwner404: every handler 404s for a non-owner (no existence
// oracle) and mutates nothing, while the owner's own requests still work.
func TestLocalFS_CrossOwner404(t *testing.T) {
	mgr, workDir := newTestDevManager(t, "sb-1", "u:alice")
	if err := os.WriteFile(filepath.Join(workDir, "greeting.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	rBob := localFSRouter(mgr, "u:bob")

	bobCases := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/files?path=new.txt", strings.NewReader("bob")),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/files?path=greeting.txt", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/filesystem/list?path=/", nil),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/mkdir", strings.NewReader(`{"path":"bobdir"}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/move", strings.NewReader(`{"source":"greeting.txt","destination":"moved-by-bob.txt"}`)),
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sb-1/filesystem/remove", strings.NewReader(`{"path":"greeting.txt"}`)),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/sb-1/filesystem/stat?path=greeting.txt", nil),
	}
	for _, req := range bobCases {
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		rBob.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("bob %s %s: got %d, want 404: %s", req.Method, req.URL.String(), w.Code, w.Body.String())
		}
	}

	// None of bob's attempts may have mutated alice's workspace.
	if _, err := os.Stat(filepath.Join(workDir, "greeting.txt")); err != nil {
		t.Fatalf("bob's cross-owner requests touched alice's file: %v", err)
	}
	for _, unexpected := range []string{"new.txt", "bobdir", "moved-by-bob.txt"} {
		if _, err := os.Stat(filepath.Join(workDir, unexpected)); !os.IsNotExist(err) {
			t.Fatalf("bob's cross-owner request created %s despite the 404", unexpected)
		}
	}

	// Regression guard: alice's own requests against her own sandbox still work.
	rAlice := localFSRouter(mgr, "u:alice")

	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1/filesystem/list?path=/", ""); w.Code != http.StatusOK {
		t.Fatalf("alice list: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1/filesystem/stat?path=greeting.txt", ""); w.Code != http.StatusOK {
		t.Fatalf("alice stat: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "GET", "/v1/sandboxes/sb-1/files?path=greeting.txt", ""); w.Code != http.StatusOK || w.Body.String() != "hi" {
		t.Fatalf("alice read: got %d %q", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "POST", "/v1/sandboxes/sb-1/files?path=new.txt", "alice"); w.Code != http.StatusOK {
		t.Fatalf("alice write: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "POST", "/v1/sandboxes/sb-1/filesystem/mkdir", `{"path":"alicedir"}`); w.Code != http.StatusOK {
		t.Fatalf("alice mkdir: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "POST", "/v1/sandboxes/sb-1/filesystem/move", `{"source":"greeting.txt","destination":"moved.txt"}`); w.Code != http.StatusOK {
		t.Fatalf("alice move: got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, rAlice, "POST", "/v1/sandboxes/sb-1/filesystem/remove", `{"path":"moved.txt"}`); w.Code != http.StatusNoContent {
		t.Fatalf("alice remove: got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "moved.txt")); !os.IsNotExist(err) {
		t.Fatalf("alice's remove did not delete moved.txt")
	}
	if _, err := os.Stat(filepath.Join(workDir, "alicedir")); err != nil {
		t.Fatalf("alice's mkdir did not create alicedir: %v", err)
	}
}

// A symlink the sandbox creates must not become a way out of the workspace,
// for reads, listings, or writes through a dangling link.
func TestLocalFS_SymlinksCannotEscape(t *testing.T) {
	mgr, workDir := newTestDevManager(t, "sb-1", "u:a")
	r := localFSRouter(mgr, "u:a")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"file": filepath.Join(outside, "secret"), "dir": outside, "dangling": filepath.Join(outside, "new"),
		"inside": filepath.Join(workDir, "real.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(workDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "real.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/sandboxes/sb-1/files?path=file"},
		{"GET", "/v1/sandboxes/sb-1/files?path=dir/secret"},
		{"GET", "/v1/sandboxes/sb-1/filesystem/list?path=dir"},
		{"POST", "/v1/sandboxes/sb-1/files?path=dangling"},
		{"POST", "/v1/sandboxes/sb-1/files?path=dir/planted"},
	} {
		if got := do(c.method, c.path, "x"); got != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", c.method, c.path, got)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("a write through the dangling link created a file outside the workspace")
	}
	if got := do("GET", "/v1/sandboxes/sb-1/files?path=inside", ""); got != http.StatusOK {
		t.Errorf("a link to a file inside the workspace = %d, want 200", got)
	}
}

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newCronSandboxWithAPI is a shared fixture (the cron name is historical): a
// server with one sandbox, plus the *Server so tests can drive it directly.
func newCronSandboxWithAPI(t *testing.T) (url, key, sandboxID string, st *store.Store, api *Server) {
	t.Helper()
	dir := t.TempDir()
	stR, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { stR.Close() })
	_, k, err := stR.CreateKey(t.Context(), "cron", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: stR},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	a := New(d, stR)
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", k, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create sandbox = %d %v", code, out)
	}
	id, _ := out["id"].(string)
	return srv.URL, k, id, stR, a
}

func newCronSandbox(t *testing.T) (srv, key, sandboxID string, st *store.Store) {
	t.Helper()
	u, k, id, stR, _ := newCronSandboxWithAPI(t)
	return u, k, id, stR
}

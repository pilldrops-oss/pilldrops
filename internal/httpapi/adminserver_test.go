package httpapi

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newAdminServer builds a node whose key carries the operator role.
func newAdminServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKeyWithRole(t.Context(), "admin", "default", "operator")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, key
}

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

// nodeServer builds a node API by hand so the test can put it in clustered mode.
func nodeServer(t *testing.T, clustered bool) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	api := New(d, st)
	if clustered {
		api.SetCluster("http://orchestrator.invalid", "k")
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return srv, opKey
}

// On a fleet node the orchestrator serves the dashboard, so a node must not be
// able to enumerate every customer through these endpoints.
func TestFleetNodeRefusesFleetWideTenantData(t *testing.T) {
	fleetWide := []string{
		"/v1/admin/users",
		"/v1/admin/overview",
		"/v1/admin/sessions",
		"/v1/admin/errors",
		"/v1/admin/funnel",
	}

	srv, opKey := nodeServer(t, true)

	for _, path := range fleetWide {
		code, _ := doOrg(t, "GET", srv.URL+path, opKey, "", "")
		if code != http.StatusNotFound {
			t.Errorf("clustered node %s = %d, want 404 — a node must not serve fleet-wide tenant data", path, code)
		}
	}
}

func TestStandaloneNodeStillServesItsDashboard(t *testing.T) {
	srv, opKey := nodeServer(t, false)
	for _, path := range []string{"/v1/admin/users", "/v1/admin/overview"} {
		code, _ := doOrg(t, "GET", srv.URL+path, opKey, "", "")
		if code == http.StatusNotFound {
			t.Errorf("standalone %s = 404 — the single-box dashboard lost an endpoint", path)
		}
	}
}

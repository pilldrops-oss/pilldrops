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

// On a clustered node the customer API accepts only operator keys. Capacity
// checks are node-local, so a tenant hitting nodes directly would collect
// their concurrency allowance once per node.

// newClusteredTestServer is newTestServerWithStore plus SetCluster, returning
// both a tenant key and an operator key.
func newClusteredTestServer(t *testing.T) (srv *httptest.Server, st *store.Store, userKey, opKey string, s *Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, userKey, err = st.CreateKey(t.Context(), "tenant", "acme"); err != nil {
		t.Fatalf("tenant key: %v", err)
	}
	if _, opKey, err = st.CreateKeyWithRole(t.Context(), "op", "default", "operator"); err != nil {
		t.Fatalf("operator key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s = New(d, st)
	attachTestFunctions(s, d, st, dir)
	s.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	s.SetCluster("http://orchestrator.invalid", "orch-key")
	srv = httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, st, userKey, opKey, s
}

// The gate lives in auth(), so it must cover every customer route, not just create.
func TestClusteredNodeRefusesTenantKeys(t *testing.T) {
	srv, _, userKey, _, _ := newClusteredTestServer(t)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/sandboxes"},
		{"GET", "/v1/sandboxes"},
		{"GET", "/v1/limits"},
		{"GET", "/v1/concurrency"},
		{"DELETE", "/v1/sandboxes/whatever"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			code, body := do(t, tc.method, srv.URL+tc.path, userKey, "{}")
			if code != http.StatusForbidden {
				t.Errorf("%s %s with a tenant key = %d, want 403 (body %q)", tc.method, tc.path, code, body)
			}
		})
	}
}

func TestClusteredNodeAcceptsTheOrchestratorsKey(t *testing.T) {
	srv, _, _, opKey, _ := newClusteredTestServer(t)

	if code, body := do(t, "GET", srv.URL+"/v1/sandboxes", opKey, ""); code != http.StatusOK {
		t.Errorf("operator key on a clustered node = %d, want 200 (body %q)", code, body)
	}
}

func TestStandaloneNodeStillServesTenantsDirectly(t *testing.T) {
	srv, userKey := newTestServer(t) // no SetCluster

	if code, body := do(t, "GET", srv.URL+"/v1/sandboxes", userKey, ""); code != http.StatusOK {
		t.Errorf("tenant key on a STANDALONE node = %d, want 200 — the gate must only apply to a fleet (body %q)", code, body)
	}
}

// An operator who exposes nodes deliberately can re-open the gate and accepts
// that per-node caps become the only caps.
func TestDirectTenantAPIEscapeHatch(t *testing.T) {
	srv, _, userKey, _, s := newClusteredTestServer(t)

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes", userKey, ""); code != http.StatusForbidden {
		t.Fatalf("precondition: tenant key should be refused, got %d", code)
	}
	s.allowDirectTenantAPI = true
	if code, body := do(t, "GET", srv.URL+"/v1/sandboxes", userKey, ""); code != http.StatusOK {
		t.Errorf("with the escape hatch set = %d, want 200 (body %q)", code, body)
	}
}

// A 403 before the credential check would tell an unauthenticated prober this is a fleet node.
func TestFleetGateRunsAfterAuthentication(t *testing.T) {
	srv, _, _, _, _ := newClusteredTestServer(t)

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes", "not-a-real-key", ""); code != http.StatusUnauthorized {
		t.Errorf("an invalid key on a clustered node = %d, want 401 — a 403 here leaks that this is a fleet node", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no key at all = %d, want 401", code)
	}
}

func TestHealthIsNotBehindTheFleetGate(t *testing.T) {
	srv, _, _, _, _ := newClusteredTestServer(t)

	for _, p := range []string{"/health", "/livez"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s on a clustered node = %d, want 200", p, resp.StatusCode)
		}
	}
}

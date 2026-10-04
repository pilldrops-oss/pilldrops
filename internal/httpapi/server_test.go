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

func newTestServer(t *testing.T) (*httptest.Server, string) {
	srv, _, key := newTestServerWithStore(t)
	return srv, key
}

// newOperatorTestServer is newTestServer with an operator key, for the
// node-wide routes a member key is refused on.
func newOperatorTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, st, _ := newTestServerWithStore(t)
	_, key, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	return srv, key
}

// testOrgOverrideSecret is the shared secret test servers verify X-Org-Override with.
const testOrgOverrideSecret = "test-org-override-secret"

// testNodeID is non-empty on purpose: an empty id on both sides would make the
// assertion's node binding vacuous.
const testNodeID = "test-node-a"

// newTestServerWithStore also returns the store so a test can mint keys with a role.
// opts adjust the daemon config before it is built, for the tiers a default
// test node deliberately does not have (an object store, say).
func newTestServerWithStore(t *testing.T, opts ...func(*daemon.Config, *store.Store)) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	cfg := daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	}
	for _, o := range opts {
		o(&cfg, st)
	}
	d := daemon.New(cfg)
	s := New(d, st)
	s.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	s.SetNodeID(testNodeID)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, st, key
}

func TestAdminRequiresOperatorRole(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	if code, _ := do(t, "GET", srv.URL+"/v1/admin/users", userKey, ""); code != http.StatusForbidden {
		t.Errorf("user key on /v1/admin/users = %d, want 403", code)
	}
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/admin/users", opKey, ""); code != 200 {
		t.Errorf("operator key on /v1/admin/users = %d, want 200", code)
	}
}

func TestHealthNoAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("health = %d, want 200", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, key := newTestServer(t)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("no-token = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer sf_live_wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong-token = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("valid-token = %d, want 200", resp.StatusCode)
	}
}

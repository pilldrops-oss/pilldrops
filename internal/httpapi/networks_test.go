package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newTagNetworkServer is newTestServer on a fake runtime whose TagNetworks bit
// the caller chooses, which is the only thing that decides 501 from 201.
func newTagNetworkServer(t *testing.T, supported bool) (*httptest.Server, *runtimetest.Fake, string) {
	t.Helper()
	fake := runtimetest.NewFake()
	fake.TagNetworks = supported
	srv, key := newFakeRuntimeServer(t, fake)
	return srv, fake, key
}

// newFakeRuntimeServer is newTestServer on a caller-configured fake runtime, so
// a test can choose the capability bits before anything serves a request.
func newFakeRuntimeServer(t *testing.T, fake *runtimetest.Fake) (*httptest.Server, string) {
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
	d := daemon.New(daemon.Config{
		Runtime:     fake,
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	s.SetNodeID(testNodeID)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, key
}

// The refusal is the point: a node that cannot build the network must not hand
// back a sandbox whose row says it is on one.
func TestCreateWithNetworksRefusedOnARuntimeWithoutTagNetworks(t *testing.T) {
	srv, _, key := newTagNetworkServer(t, false)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","networks":["web"]}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("create with networks returned %d (%v), want 501", code, out)
	}
	if out["code"] != codeTagNetworksUnsupported {
		t.Errorf("code = %v, want %q", out["code"], codeTagNetworksUnsupported)
	}
	if msg, _ := out["error"].(string); msg == "" || !contains(msg, "fake") {
		t.Errorf("error %q does not name the runtime, so the caller cannot tell which node refused", msg)
	}
	// Controls: no networks at all, and the explicitly empty list, both boot.
	for _, body := range []string{`{"template":"base"}`, `{"template":"base","networks":[]}`} {
		if code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, body); code != 201 {
			t.Fatalf("create %s returned %d (%v), want 201 — absent and [] are both untagged", body, code, out)
		}
	}
}

func TestCreateAndPatchNetworksOnACapableRuntime(t *testing.T) {
	srv, fake, key := newTagNetworkServer(t, true)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","networks":["web","db"]}`)
	if code != 201 {
		t.Fatalf("create with networks returned %d (%v), want 201", code, out)
	}
	id, _ := out["id"].(string)
	// The runtime is handed org-scoped keys, never the bare tag.
	if got := fake.NetworksOf(id); len(got) != 2 || got[0] != "default/web" || got[1] != "default/db" {
		t.Fatalf("runtime received %q, want the org-scoped keys default/web and default/db", got)
	}

	code, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if code != 200 {
		t.Fatalf("get returned %d", code)
	}
	if out["private_ip"] != "10.128.0.2" || out["node"] != testNodeID {
		t.Errorf("get = %v, want private_ip and node so an unreachable cross-node peer is explainable", out)
	}

	// A PATCH replaces the membership rather than adding to it.
	if code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"networks":["db"]}`); code != 200 {
		t.Fatalf("patch networks returned %d (%v), want 200", code, out)
	}
	if got := fake.NetworksOf(id); len(got) != 1 || got[0] != "default/db" {
		t.Fatalf("after PATCH the runtime holds %q, want only default/db — a patch replaces", got)
	}

	// Clearing closes the sandbox off again, and the response stops claiming a network.
	if code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"networks":[]}`); code != 200 {
		t.Fatalf("patch to [] returned %d (%v), want 200", code, out)
	}
	if got := fake.NetworksOf(id); got != nil {
		t.Fatalf("after clearing, the runtime still holds %q", got)
	}
	_, out = do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if _, ok := out["networks"]; ok {
		t.Errorf("get still reports networks after a clear: %v", out)
	}
	if _, ok := out["private_ip"]; ok {
		t.Errorf("get still reports a private_ip for a sandbox on no network: %v", out)
	}

	// An invalid tag is a 400 naming the rule, not a 501.
	if code, out := do(t, "PATCH", srv.URL+"/v1/sandboxes/"+id, key, `{"networks":["a b"]}`); code != 400 {
		t.Fatalf("patch with an invalid tag returned %d (%v), want 400", code, out)
	}
}

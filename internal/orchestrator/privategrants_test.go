package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

type privateFakeNode struct {
	id, key string
	mu      sync.Mutex
	patches map[string]string
	deleted []string
}

func (n *privateFakeNode) lastPatch(t *testing.T, id string) string {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	got, ok := n.patches[id]
	if !ok {
		t.Fatalf("node got no PATCH for %s", id)
	}
	return got
}

func (n *privateFakeNode) wasDeleted(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, d := range n.deleted {
		if d == id {
			return true
		}
	}
	return false
}

// newFakeNode registers a node, and sandbox sb1 on it when place is true. A node
// that does not support private access answers PATCH with 409.
func (ts *connectorTestServer) newFakeNode(t *testing.T, supports, place bool) *privateFakeNode {
	t.Helper()
	n := &privateFakeNode{id: "n1", patches: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == "PATCH" && !supports:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"private_access_unsupported"}`))
		case r.Method == "PATCH":
			n.mu.Lock()
			n.patches[strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")] = string(b)
			n.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "DELETE":
			n.mu.Lock()
			n.deleted = append(n.deleted, strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"))
			n.mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"destroyed"}`))
		case r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sbnew","template":"base","status":"running"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	n.key = ts.s.nodeKeyFor(n.id, "")
	if err := ts.s.reg.Upsert(cluster.NodeUpsert{ID: n.id, URL: srv.URL, Key: n.key,
		Cap: cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}}, ts.s.now()); err != nil {
		t.Fatal(err)
	}
	ts.s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: "docker", PrivateAccess: supports})
	if place {
		ts.s.reg.Place("sb1", n.id)
	}
	rec := store.SandboxRecord{ID: "sb1", OrgID: "org-a", Template: "base", State: "running"}
	if !place {
		rec.State = "paused"
	}
	if err := ts.s.st.Load().SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	return n
}

func (ts *connectorTestServer) fakeNode(t *testing.T, supports bool) *privateFakeNode {
	return ts.newFakeNode(t, supports, true)
}

// fakeNodePaused leaves sb1 stored but placed on no node.
func (ts *connectorTestServer) fakeNodePaused(t *testing.T) {
	ts.newFakeNode(t, true, false)
}

func (ts *connectorTestServer) announce(t *testing.T, connector, targets string) {
	t.Helper()
	dialInstance(t, ts, connector, "inst-aaaaaaaaaaaa", targets, true)
}

func TestGrantStoresAndPushes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432},{"name":"api","port":443,"host":"internal.corp"}]`)
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"api"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	got := node.lastPatch(t, "sb1")
	if !strings.Contains(got, `"private":[{"name":"api","host":"internal.corp","port":443}]`) {
		t.Fatalf("node got %s", got)
	}
}

func TestGrantRefusals(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"lab"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	ts.announce(t, "lab", `[{"name":"db","port":5432}]`)
	cases := []struct {
		key, body string
		want      int
	}{
		{ts.sandboxToken, `{"connector":"prod","target":"db"}`, http.StatusForbidden},
		{ts.otherOrgKey, `{"connector":"prod","target":"db"}`, http.StatusNotFound},
		{ts.key, `{"connector":"prod","target":"nope"}`, http.StatusNotFound},
		{ts.key, `{"connector":"ghost","target":"db"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", c.key, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first grant: %d", resp.StatusCode)
	}
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"lab","target":"db"}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(ts.body(resp), "private_name_taken") {
		t.Fatalf("second db: %d", resp.StatusCode)
	}
}

func TestGrantOnNodeWithoutSupportRollsBack(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNode(t, false)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`)
	body := strings.ToLower(ts.body(resp))
	if resp.StatusCode != http.StatusConflict || strings.Contains(body, "docker") || strings.Contains(body, "firecracker") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if gs, _ := ts.s.st.Load().PrivateGrants(t.Context(), "sb1"); len(gs) != 0 {
		t.Fatal("grant kept after the node refused")
	}
}

func TestGrantWhilePausedAppliesOnResume(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNodePaused(t)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant while paused: %d", resp.StatusCode)
	}
	rec, _, _ := ts.s.st.Load().SandboxByID(t.Context(), "sb1")
	if !strings.Contains(rec.Private, `"db"`) {
		t.Fatalf("private_targets not stored for resume: %q", rec.Private)
	}
}

func TestRevokeConnectorRepushesSandboxes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`)
	ts.do(t, "DELETE", "/v1/connectors/"+ts.connectorID(t, "prod"), ts.key, "")
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"private":[]`) {
		t.Fatalf("node not cleared: %s", got)
	}
}

func TestCreateWithPrivateGrantsTheNewSandbox(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	resp := ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","private":[{"connector":"prod","target":"db"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, ts.body(resp))
	}
	if got := node.lastPatch(t, "sbnew"); !strings.Contains(got, `"name":"db"`) {
		t.Fatalf("node got %s", got)
	}
}

func TestCreateWithFailingGrantDestroysTheSandbox(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	resp := ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","private":[{"connector":"prod","target":"nope"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create: %d %s", resp.StatusCode, ts.body(resp))
	}
	if !node.wasDeleted("sbnew") {
		t.Fatal("sandbox kept after its grant failed")
	}
	if _, ok := ts.s.reg.NodeFor("sbnew"); ok {
		t.Fatal("destroyed sandbox still placed")
	}
}

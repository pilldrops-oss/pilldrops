package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

type privateFakeNode struct {
	id, key      string
	mu           sync.Mutex
	patches      map[string]string
	deleted      []string
	failDelete   bool
	creates      []string
	onPatch      func(body string) // runs before the PATCH is recorded
	createAnswer string            // POST reply; "" is the default sbnew
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

// newFakeNode registers node n1, and sandbox sb1 on it when place is true.
func (ts *connectorTestServer) newFakeNode(t *testing.T, supports, place bool) *privateFakeNode {
	t.Helper()
	n := ts.fakeNodeNamed(t, "n1", supports)
	rec := store.SandboxRecord{ID: "sb1", OrgID: "org-a", Template: "base", State: "running", NodeID: n.id}
	if place {
		ts.s.reg.Place("sb1", n.id)
	} else {
		rec.State, rec.NodeID = "paused", ""
	}
	if err := ts.s.st.Load().SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	return n
}

// fakeNodeNamed registers a node that runs no sandbox. A node that does not
// support private access answers PATCH with 409.
func (ts *connectorTestServer) fakeNodeNamed(t *testing.T, id string, supports bool) *privateFakeNode {
	t.Helper()
	n := &privateFakeNode{id: id, patches: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == "PATCH" && !supports:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"private_access_unsupported"}`))
		case r.Method == "PATCH":
			if n.onPatch != nil {
				n.onPatch(string(b))
			}
			n.mu.Lock()
			n.patches[strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")] = string(b)
			n.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "DELETE" && n.failDelete:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "DELETE":
			n.mu.Lock()
			n.deleted = append(n.deleted, strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"))
			n.mu.Unlock()
			_, _ = w.Write([]byte(`{"status":"destroyed"}`))
		case r.Method == "POST":
			n.mu.Lock()
			n.creates = append(n.creates, string(b))
			ans := n.createAnswer
			n.mu.Unlock()
			if ans == "" {
				ans = `{"id":"sbnew","template":"base","status":"running"}`
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(ans))
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
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1"); len(gs) != 0 {
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

func TestCreateForwardsBodyWithoutPrivate(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	ts.body(ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","private":[{"connector":"prod","target":"db"}]}`))
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.creates) != 1 || strings.Contains(node.creates[0], "private") || !strings.Contains(node.creates[0], `"template"`) {
		t.Fatalf("node create bodies: %v", node.creates)
	}
}

func TestCreateCleanupSurvivesNodeDeleteFailure(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	node.failDelete = true
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	resp := ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","private":[{"connector":"prod","target":"db"},{"connector":"prod","target":"nope"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create: %d %s", resp.StatusCode, ts.body(resp))
	}
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sbnew"); len(gs) != 0 {
		t.Fatalf("grants left after cleanup: %v", gs)
	}
	if _, ok := ts.s.reg.NodeFor("sbnew"); ok {
		t.Fatal("sandbox still placed")
	}
}

func TestGrantBySlug(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNode(t, true)
	if err := ts.s.st.Load().SaveSandbox(t.Context(), store.SandboxRecord{ID: "sb1", OrgID: "org-a", Name: "my-box", Slug: "my-box", Template: "base", State: "running"}); err != nil {
		t.Fatal(err)
	}
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	if resp := ts.do(t, "POST", "/v1/sandboxes/my-box/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant by slug: %d %s", resp.StatusCode, ts.body(resp))
	}
}

func TestGrantListAndRevokeRefusals(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432},{"name":"api","port":443}]`)
	for _, target := range []string{"db", "api"} {
		ts.body(ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"`+target+`"}`))
	}
	for _, c := range []struct {
		method, path, key string
		want              int
	}{
		{"GET", "/v1/sandboxes/sb1/private", ts.sandboxToken, http.StatusForbidden},
		{"GET", "/v1/sandboxes/sb1/private", ts.otherOrgKey, http.StatusNotFound},
		{"DELETE", "/v1/sandboxes/sb1/private/prod/db", ts.sandboxToken, http.StatusForbidden},
		{"DELETE", "/v1/sandboxes/sb1/private/prod/db", ts.otherOrgKey, http.StatusNotFound},
	} {
		if resp := ts.do(t, c.method, c.path, c.key, ""); resp.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/prod/db", ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	got := node.lastPatch(t, "sb1")
	if !strings.Contains(got, `"api"`) || strings.Contains(got, `"db"`) {
		t.Fatalf("node not re-pushed with the shorter list: %s", got)
	}
}

func TestGrantListOffAnswers501(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNode(t, true)
	t.Setenv("PRIVATE_INGRESS", "")
	resp := ts.do(t, "GET", "/v1/sandboxes/sb1/private", ts.key, "")
	if b := ts.body(resp); resp.StatusCode != http.StatusNotImplemented || !strings.Contains(b, "no_private_ingress") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestDestroyDropsGrants(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	ts.body(ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`))
	if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1", ts.key, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("destroy: %d %s", resp.StatusCode, ts.body(resp))
	}
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1"); len(gs) != 0 {
		t.Fatalf("grants left: %v", gs)
	}
}

// A grant's push held at the node must not land after a later revoke's push.
func TestConcurrentGrantAndRevokeLeaveTheNodeCurrent(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432},{"name":"api","port":443}]`)
	ts.body(ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`))

	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	node.onPatch = func(body string) {
		if strings.Contains(body, `"api"`) {
			once.Do(func() { close(held); <-release })
		}
	}
	send := func(method, path, body string) chan int {
		done := make(chan int, 1)
		req, _ := http.NewRequest(method, ts.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ts.key)
		go func() {
			resp, err := ts.srv.Client().Do(req)
			if err != nil {
				done <- 0
				return
			}
			_ = resp.Body.Close()
			done <- resp.StatusCode
		}()
		return done
	}
	granted := send("POST", "/v1/sandboxes/sb1/private", `{"connector":"prod","target":"api"}`)
	<-held
	revoked := send("DELETE", "/v1/sandboxes/sb1/private/prod/api", "")
	revokeCode := 0
	select {
	case revokeCode = <-revoked: // unserialized: the revoke's push reached the node first
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-granted; code != http.StatusCreated {
		t.Fatalf("grant: %d", code)
	}
	if revokeCode == 0 {
		revokeCode = <-revoked
	}
	if revokeCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", revokeCode)
	}
	if got := node.lastPatch(t, "sb1"); strings.Contains(got, `"api"`) || !strings.Contains(got, `"db"`) {
		t.Fatalf("node left with a stale target list: %s", got)
	}
}

func TestSandboxLocksSerializeAndForgetIdleEntries(t *testing.T) {
	var l sandboxLocks
	unlock := l.lock("sb1")
	got := make(chan struct{})
	go func() { l.lock("sb1")(); close(got) }()
	select {
	case <-got:
		t.Fatal("a second holder got the lock")
	case <-time.After(50 * time.Millisecond):
	}
	l.lock("sb2")() // another sandbox does not wait
	unlock()
	<-got
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) != 0 {
		t.Fatalf("idle entries kept: %d", len(l.m))
	}
}

// Two targets announcing one hostname would share one address in the sandbox.
func TestGrantRefusesASecondTargetWithTheSameHost(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"lab"}`)
	ts.announce(t, "prod", `[{"name":"api","port":443,"host":"internal.corp"}]`)
	ts.announce(t, "lab", `[{"name":"web","port":443,"host":"Internal.Corp."}]`)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"api"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"lab","target":"web"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "private_name_taken") || !strings.Contains(b, "internal.corp") {
		t.Fatalf("second grant on one host: %d %s", resp.StatusCode, b)
	}
	if got := node.lastPatch(t, "sb1"); strings.Contains(got, `"web"`) {
		t.Fatalf("refused grant reached the node: %s", got)
	}

	resp = ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","private":[{"connector":"prod","target":"api"},{"connector":"lab","target":"web"}]}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "private_name_taken") {
		t.Fatalf("create with two targets on one host: %d %s", resp.StatusCode, b)
	}
	if !node.wasDeleted("sbnew") {
		t.Fatal("sandbox kept after its grant was refused")
	}
}

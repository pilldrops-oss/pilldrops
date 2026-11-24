package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func (n *privateFakeNode) patchOf(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.patches[id]
}

// putInTag stores a sandbox row with tags, as its node would after a create or
// a PATCH, and places it on nodeID when that is set.
func (ts *connectorTestServer) putInTag(t *testing.T, id, nodeID string, tags ...string) {
	t.Helper()
	if err := saveTags(ts, id, nodeID, tags...); err != nil {
		t.Fatal(err)
	}
	if nodeID != "" {
		ts.s.reg.Place(id, nodeID)
	}
}

func saveTags(ts *connectorTestServer, id, nodeID string, tags ...string) error {
	nets, _ := json.Marshal(append([]string{}, tags...))
	return ts.s.st.Load().SaveSandbox(context.Background(), store.SandboxRecord{ID: id, OrgID: "org-a", Template: "base",
		State: "running", NodeID: nodeID, Networks: string(nets)})
}

const routesCaps = `{"targets":[],"subnets":["10.0.0.0/16"],"dns":{"suffixes":["corp.internal"]}}`

func TestTagGrantReachesMembersAndLaterJoiners(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.announceRoutes(t, "prod", routesCaps)
	resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"sandboxes":1`) || !strings.Contains(b, `"tag":"backend"`) || !strings.Contains(b, `"unsupported":[]`) {
		t.Fatalf("tag grant: %d %s", resp.StatusCode, b)
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb1"), `"10.0.5.0/24"`) })
	if b := ts.body(ts.do(t, "GET", "/v1/sandboxes/sb1/private", ts.key, "")); !strings.Contains(b, `"source":"tag:backend"`) {
		t.Fatalf("effective list: %s", b)
	}

	// Leaving: the node persists the row, then the control plane re-pushes.
	node.onPatch = func(body string) {
		if strings.Contains(body, `"networks":[]`) {
			_ = saveTags(ts, "sb1", node.id)
		}
	}
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb1", ts.key, `{"networks":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("leave: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb1"), `"subnets":[]`) })

	node.onPatch = func(body string) {
		if strings.Contains(body, `"networks":["backend"]`) {
			_ = saveTags(ts, "sb1", node.id, "backend")
		}
	}
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb1", ts.key, `{"networks":["backend"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("join: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb1"), `"10.0.5.0/24"`) })
}

// Review focus 1: a big tag is pushed in the background, a few at a time, and a
// per-sandbox grant made meanwhile is not lost to a stale push.
func TestTagGrantFansOutBounded(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	var mu sync.Mutex
	active, peak := 0, 0
	node.onPatch = func(string) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}
	ids := make([]string, 30)
	for i := range ids {
		ids[i] = fmt.Sprintf("sbf%02d", i)
		ts.putInTag(t, ids[i], node.id, "fleet")
	}
	ts.announceRoutes(t, "prod", routesCaps)
	resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"fleet","connector":"prod","subnet":"10.0.5.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"sandboxes":30`) {
		t.Fatalf("tag grant: %d %s", resp.StatusCode, b)
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes/"+ids[0]+"/private", ts.key, `{"connector":"prod","dns":"corp.internal"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("own grant during the fan-out: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool {
		for _, id := range ids {
			if !strings.Contains(node.patchOf(id), `"10.0.5.0/24"`) {
				return false
			}
		}
		return true
	})
	waitFor(t, func() bool {
		p := node.patchOf(ids[0])
		return strings.Contains(p, `"10.0.5.0/24"`) && strings.Contains(p, `"corp.internal"`)
	})
	mu.Lock()
	defer mu.Unlock()
	// The own grant's push runs beside the fan-out, so one more than its bound.
	if peak > tagPushConcurrency+1 || peak < 2 {
		t.Fatalf("peak concurrent pushes %d, want 2..%d", peak, tagPushConcurrency+1)
	}
}

func TestTagGrantRefusesAConflictWithAMember(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/16"]}`)
	ts.announceRoutes(t, "lab", `{"targets":[],"subnets":["10.0.0.0/8"]}`)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.0.0/16"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("own grant: %d", resp.StatusCode)
	}
	resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"lab","subnet":"10.0.5.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "private_route_conflict") || !strings.Contains(b, "sb1") {
		t.Fatalf("conflicting tag grant: %d %s", resp.StatusCode, b)
	}
	if gs, _ := ts.s.st.Load().TagGrants(t.Context(), "org-a", "backend"); len(gs) != 0 {
		t.Fatalf("stored %+v", gs)
	}
}

func TestTagJoinRefusedWhereTheNodeCannotRoute(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true) // private access, no routes
	ts.putInTag(t, "sb2", node.id, "backend")
	ts.announceRoutes(t, "prod", routesCaps)
	resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"unsupported":["sb2"]`) {
		t.Fatalf("tag grant: %d %s", resp.StatusCode, b)
	}
	resp = ts.do(t, "PATCH", "/v1/sandboxes/sb1", ts.key, `{"networks":["backend"]}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "private_routes_unsupported") {
		t.Fatalf("join on an old node: %d %s", resp.StatusCode, b)
	}
	if got := node.patchOf("sb1"); got != "" {
		t.Fatalf("the refused join reached the node: %s", got)
	}
}

func TestTagGrantListRevokeAndPermissions(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.announceRoutes(t, "prod", routesCaps)
	const body = `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.sandboxToken, body); resp.StatusCode != http.StatusForbidden {
		t.Errorf("sandbox token grant: %d", resp.StatusCode)
	}
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.otherOrgKey, body); resp.StatusCode != http.StatusNotFound {
		t.Errorf("another org's grant: %d", resp.StatusCode)
	}
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"Back End","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad tag: %d", resp.StatusCode)
	}
	id := ts.jsonField(t, ts.do(t, "POST", "/v1/private/grants", ts.key, body), "id")
	if b := ts.body(ts.do(t, "GET", "/v1/private/grants?tag=backend", ts.key, "")); !strings.Contains(b, id) {
		t.Fatalf("list: %s", b)
	}
	if b := ts.body(ts.do(t, "GET", "/v1/private/grants", ts.otherOrgKey, "")); strings.Contains(b, id) {
		t.Fatalf("another org listed it: %s", b)
	}
	if resp := ts.do(t, "GET", "/v1/private/grants", ts.sandboxToken, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("sandbox token list: %d", resp.StatusCode)
	}
	own := ts.jsonField(t, ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","dns":"corp.internal"}`), "id")
	for _, c := range []struct {
		key, id string
		want    int
	}{{ts.otherOrgKey, id, http.StatusNotFound}, {ts.key, own, http.StatusNotFound}, {ts.key, id, http.StatusNoContent}} {
		if resp := ts.do(t, "DELETE", "/v1/private/grants/"+c.id, c.key, ""); resp.StatusCode != c.want {
			t.Errorf("revoke %s: %d, want %d", c.id, resp.StatusCode, c.want)
		}
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb1"), `"subnets":[]`) })
}

func TestCreateInATagGetsItsRoutes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	node.createAnswer = `{"id":"sbnew","template":"base","status":"running","networks":["backend"]}`
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d", resp.StatusCode)
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes", ts.key, `{"template":"base","networks":["backend"]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sbnew"), `"10.0.5.0/24"`) })
}

func TestTagGrantLimitPerOrg(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.routesNode(t)
	ts.announceRoutes(t, "prod", routesCaps)
	old := maxTagGrantsPerOrg
	maxTagGrantsPerOrg = 2
	t.Cleanup(func() { maxTagGrantsPerOrg = old })
	for i, want := range []int{http.StatusCreated, http.StatusCreated, http.StatusTooManyRequests} {
		body := fmt.Sprintf(`{"tag":"t%d","connector":"prod","subnet":"10.0.5.0/24"}`, i)
		if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, body); resp.StatusCode != want {
			t.Errorf("grant %d: %d, want %d", i, resp.StatusCode, want)
		}
	}
}

func TestMoveAndRehomeOfARoutedSandboxNeedPrivateRoutes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true) // private access, no routes
	ctx := context.WithValue(withOrg(withOperator(t.Context()), "org-a"), moveCtxKey{}, moveCtx{from: "elsewhere", routes: true})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/sandboxes", bytes.NewReader([]byte(`{"template":"base","id":"sb1"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ts.s.handleCreate(w, req)
	node.mu.Lock()
	creates := len(node.creates)
	node.mu.Unlock()
	if creates != 0 || w.Code < 300 {
		t.Fatalf("a routed move reached a node without private_routes: %d %s", w.Code, w.Body.String())
	}

	rec, _, _ := ts.s.st.Load().SandboxByID(t.Context(), "sb-1-conn")
	rec.PrivateRoutes = `{"subnets":["10.0.5.0/24"],"dns":[]}`
	rr := httptest.NewRequestWithContext(withOperator(t.Context()), http.MethodPost, "/", nil)
	if _, ok := ts.s.rehome(httptest.NewRecorder(), rr, rec); ok {
		t.Fatal("rehomed a routed sandbox onto a node without private_routes")
	}
	capable := ts.fakeNodeNamed(t, "capable", true)
	ts.s.reg.SetNodeMeta(capable.id, cluster.NodeMeta{Runtime: "docker", PrivateAccess: true, PrivateRoutes: true})
	if n, ok := ts.s.rehome(httptest.NewRecorder(), rr, rec); !ok || n.ID != capable.id {
		t.Fatalf("rehome = %v %v, want the capable node", n, ok)
	}
}

func TestTagChangeOnOldNodeKeepsExistingTag(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true) // private access, no routes
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb1", ts.key, `{"networks":["backend","extra"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("adding an unrelated tag: %d %s", resp.StatusCode, ts.body(resp))
	}
}

// A tag revoke whose member push fails still cuts that member's connections.
func TestTagRevokeClosesConnWhenThePushFails(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432}]`)
	id := ts.jsonField(t, ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","target":"db"}`), "id")
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb1"), `"db"`) })
	c := ts.nodeDial(t, node.key, "sb1", "db", 5432)
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_private BEFORE UPDATE ON sandboxes BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if resp := ts.do(t, "DELETE", "/v1/private/grants/"+id, ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", resp.StatusCode, ts.body(resp))
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("conn survived revoke")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("conn not closed within 2s of revoke")
	}
}

// Joining a tag that carries grants is a grant, so it is held to the grant
// routes' rule: never a sandbox token, on a team only a signed-in owner or admin.
func TestTagJoinWithGrantsNeedsPrivateAdmin(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.s.reg.Place("sb-1-conn", node.id)
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d", resp.StatusCode)
	}
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/v1/sandboxes/sb-1-conn", `{"networks":["backend"]}`},
		{"POST", "/v1/sandboxes", `{"template":"base","networks":["backend"]}`},
		{"POST", "/v1/sandboxes/sb1/fork", `{}`},
	} {
		if resp := ts.do(t, c.method, c.path, ts.sandboxToken, c.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("sandbox token %s %s: %d %s", c.method, c.path, resp.StatusCode, ts.body(resp))
		}
	}
	if got := node.patchOf("sb-1-conn"); got != "" {
		t.Fatalf("the refused join reached the node: %s", got)
	}
	node.mu.Lock()
	n := len(node.creates)
	node.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused create or fork reached the node %d times", n)
	}
	// A tag without grants is as before.
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb-1-conn", ts.sandboxToken, `{"networks":["plain"]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("sandbox token joining a plain tag: %d %s", resp.StatusCode, ts.body(resp))
	}
}

// OSS privateAdmin refuses only a sandbox token, so the org's API key may join.
func TestTagJoinWithGrantsAllowsAnAPIKey(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.s.reg.Place("sb-1-conn", node.id)
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d", resp.StatusCode)
	}
	const join = `{"networks":["backend"]}`
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb-1-conn", ts.sandboxToken, join); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sandbox token join: %d %s", resp.StatusCode, ts.body(resp))
	}
	node.onPatch = func(body string) {
		if strings.Contains(body, join) {
			_ = saveTags(ts, "sb-1-conn", node.id, "backend")
		}
	}
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb-1-conn", ts.key, join); resp.StatusCode != http.StatusOK {
		t.Fatalf("API key join: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sb-1-conn"), `"10.0.5.0/24"`) })
}

// Tag grants follow the tag, so a fork of a member is routed like the member.
func TestForkOfATagMemberGetsItsRoutes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.putInTag(t, "sb1", node.id, "backend")
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d", resp.StatusCode)
	}
	// The node writes the child's row, tags and all, before it answers.
	ts.putInTag(t, "sbnew", "", "backend")
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/fork", ts.key, `{}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork: %d %s", resp.StatusCode, ts.body(resp))
	}
	waitFor(t, func() bool { return strings.Contains(node.patchOf("sbnew"), `"10.0.5.0/24"`) })
}

// A grant joined while private access is off would go live once it is on.
func TestTagJoinGateHoldsWithPrivateAccessOff(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.s.reg.Place("sb-1-conn", node.id)
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/private/grants", ts.key, `{"tag":"backend","connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("tag grant: %d", resp.StatusCode)
	}
	t.Setenv("PRIVATE_INGRESS", "")
	if resp := ts.do(t, "PATCH", "/v1/sandboxes/sb-1-conn", ts.sandboxToken, `{"networks":["backend"]}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sandbox token join with private access off: %d %s", resp.StatusCode, ts.body(resp))
	}
}

func TestForkWithoutAParentRowIsRefused(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.s.reg.Place("sbghost", node.id)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sbghost/fork", ts.key, `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fork of a sandbox with no row: %d %s", resp.StatusCode, ts.body(resp))
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if len(node.creates) != 0 {
		t.Fatal("the refused fork reached the node")
	}
}

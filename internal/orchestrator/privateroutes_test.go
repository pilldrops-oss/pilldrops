package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// routesNode is fakeNode on a node that also carries subnet and DNS grants.
func (ts *connectorTestServer) routesNode(t *testing.T) *privateFakeNode {
	t.Helper()
	n := ts.fakeNode(t, true)
	ts.s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: "docker", PrivateAccess: true, PrivateRoutes: true})
	return n
}

func (ts *connectorTestServer) announceRoutes(t *testing.T, connector, caps string) *fakeInstance {
	t.Helper()
	return dialInstanceWith(t, ts, connector, "inst-aaaaaaaaaaaa", caps, true, nil)
}

func storedRoutes(t *testing.T, ts *connectorTestServer, id string) sandbox.PrivateRoutes {
	t.Helper()
	rec, _, err := ts.s.st.Load().SandboxByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var r sandbox.PrivateRoutes
	_ = json.Unmarshal([]byte(rec.PrivateRoutes), &r)
	return r
}

func TestGrantConflict(t *testing.T) {
	t.Parallel()
	g := func(conn, kind, value string) store.Grant {
		return store.Grant{ConnectorID: conn, Kind: kind, Value: value}
	}
	host := func(g store.Grant) string {
		if g.Value == "api" || g.Value == "web" {
			return "internal.corp"
		}
		return ""
	}
	for _, c := range []struct {
		gs   []store.Grant
		code string
	}{
		{[]store.Grant{g("a", "target", "db"), g("a", "target", "db")}, ""},
		{[]store.Grant{g("a", "target", "db"), g("b", "target", "db")}, "private_name_taken"},
		{[]store.Grant{g("a", "target", "api"), g("b", "target", "web")}, "private_name_taken"},
		{[]store.Grant{g("a", "subnet", "10.0.0.0/16"), g("a", "subnet", "10.0.5.0/24")}, ""},
		{[]store.Grant{g("a", "subnet", "10.0.0.0/16"), g("b", "subnet", "10.0.5.0/24")}, "private_route_conflict"},
		{[]store.Grant{g("a", "subnet", "10.0.0.0/16"), g("b", "subnet", "10.1.0.0/16")}, ""},
		{[]store.Grant{g("a", "dns", "corp.internal"), g("b", "dns", "corp.internal")}, "private_route_conflict"},
		{[]store.Grant{g("a", "dns", "corp.internal"), g("b", "dns", "wiki.corp.internal")}, ""},
		// A conflict already in place (say, after a re-announce) does not block an unrelated grant.
		{[]store.Grant{g("a", "target", "db"), g("b", "target", "db"), g("a", "subnet", "10.0.0.0/16")}, ""},
		{[]store.Grant{g("a", "subnet", "10.0.0.0/16"), g("b", "subnet", "10.0.0.0/16"), g("b", "subnet", "10.0.5.0/24")}, "private_route_conflict"},
	} {
		if code, _ := grantConflict(c.gs, host); code != c.code {
			t.Errorf("%+v: %q, want %q", c.gs, code, c.code)
		}
	}
}

func TestSubnetAndDNSGrantsPushRoutes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/16"],"dns":{"suffixes":["corp.internal"]}}`)
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.5.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"kind":"subnet"`) || !strings.Contains(b, `"id":"pg_`) {
		t.Fatalf("subnet grant: %d %s", resp.StatusCode, b)
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"private":[]`) || !strings.Contains(got, `"private_routes":{"subnets":["10.0.5.0/24"],"dns":[]}`) {
		t.Fatalf("node got %s", got)
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","dns":"Wiki.Corp.Internal."}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("dns grant: %d %s", resp.StatusCode, ts.body(resp))
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"dns":["wiki.corp.internal"]`) {
		t.Fatalf("node got %s", got)
	}
	if r := storedRoutes(t, ts, "sb1"); len(r.Subnets) != 1 || len(r.DNS) != 1 {
		t.Fatalf("stored routes %+v", r)
	}
}

func TestGrantMustLieInsideTheAnnouncement(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/16"],"dns":{"suffixes":["corp.internal"]}}`)
	for body, want := range map[string]int{
		`{"connector":"prod","subnet":"10.1.0.0/24"}`:               http.StatusNotFound,
		`{"connector":"prod","subnet":"10.0.0.0/8"}`:                http.StatusNotFound,
		`{"connector":"prod","dns":"other.internal"}`:               http.StatusNotFound,
		`{"connector":"prod","subnet":"10.0.5.1/24"}`:               http.StatusBadRequest,
		`{"connector":"prod","target":"db","subnet":"10.0.5.0/24"}`: http.StatusBadRequest,
		`{"connector":"prod"}`:                                      http.StatusBadRequest,
		`{"connector":"ghost","subnet":"10.0.5.0/24"}`:              http.StatusNotFound,
	} {
		if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, body); resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", body, resp.StatusCode, want)
		}
	}
}

func TestSubnetGrantShadowedAndFullyReserved(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/8","169.254.0.0/16"]}`)
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.0.0/8"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusCreated || !strings.Contains(b, `"shadowed":["10.128.0.0/16"]`) {
		t.Fatalf("partly shadowed: %d %s", resp.StatusCode, b)
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"10.0.0.0/9"`) || strings.Contains(got, `"10.128.0.0/16"`) {
		t.Fatalf("node got %s", got)
	}
	resp = ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"169.254.169.0/24"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "reserved by the platform") {
		t.Fatalf("wholly reserved: %d %s", resp.StatusCode, b)
	}
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1"); len(gs) != 1 {
		t.Fatalf("grants %+v", gs)
	}
}

// An older node ignores private_routes and answers 200; the grant must not look applied.
func TestRoutesToOldNodeRefusedTargetsStillPushed(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true) // private_access only
	ts.announceRoutes(t, "prod", `{"targets":[{"name":"db","port":5432}],"subnets":["10.0.0.0/16"]}`)
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.5.0/24"}`)
	b := strings.ToLower(ts.body(resp))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(b, "private_routes_unsupported") || strings.Contains(b, "docker") || strings.Contains(b, "firecracker") {
		t.Fatalf("subnet on an old node: %d %s", resp.StatusCode, b)
	}
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1"); len(gs) != 0 {
		t.Fatalf("grant kept: %+v", gs)
	}
	if got := node.lastPatch(t, "sb1"); strings.Contains(got, "private_routes") {
		t.Fatalf("private_routes sent to a node without the capability: %s", got)
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("target on an old node: %d %s", resp.StatusCode, ts.body(resp))
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"name":"db"`) || strings.Contains(got, "private_routes") {
		t.Fatalf("node got %s", got)
	}
	if r := storedRoutes(t, ts, "sb1"); !r.Empty() {
		t.Fatalf("stored routes %+v", r)
	}
}

func TestOneAddressNeverRoutesThroughTwoConnectors(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/16"],"dns":{"suffixes":["corp.internal"]}}`)
	ts.announceRoutes(t, "lab", `{"targets":[],"subnets":["10.0.0.0/8"],"dns":{"suffixes":["corp.internal"]}}`)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"connector":"prod","subnet":"10.0.5.0/24"}`, http.StatusCreated},
		{`{"connector":"lab","subnet":"10.0.0.0/16"}`, http.StatusConflict},
		{`{"connector":"lab","subnet":"10.1.0.0/16"}`, http.StatusCreated},
		{`{"connector":"prod","dns":"corp.internal"}`, http.StatusCreated},
		{`{"connector":"lab","dns":"corp.internal"}`, http.StatusConflict},
		{`{"connector":"lab","dns":"wiki.corp.internal"}`, http.StatusCreated},
	} {
		resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, c.body)
		b := ts.body(resp)
		if resp.StatusCode != c.want || (c.want == http.StatusConflict && !strings.Contains(b, "private_route_conflict")) {
			t.Errorf("%s: %d %s, want %d", c.body, resp.StatusCode, b, c.want)
		}
	}
}

func TestRevokeSubnetGrantByID(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/16"]}`)
	id := ts.jsonField(t, ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.5.0/24"}`), "id")
	if b := ts.body(ts.do(t, "GET", "/v1/sandboxes/sb1/private", ts.key, "")); !strings.Contains(b, `"source":"sandbox"`) || !strings.Contains(b, `"routes_supported":true`) || !strings.Contains(b, id) {
		t.Fatalf("list: %s", b)
	}
	for key, want := range map[string]int{ts.sandboxToken: http.StatusForbidden, ts.otherOrgKey: http.StatusNotFound, ts.key: http.StatusNoContent} {
		if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/"+id, key, ""); resp.StatusCode != want {
			t.Errorf("revoke: %d, want %d", resp.StatusCode, want)
		}
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"subnets":[]`) {
		t.Fatalf("node got %s", got)
	}
	if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/"+id, ts.key, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke: %d", resp.StatusCode)
	}
}

func TestGrantLimitCountsEveryKind(t *testing.T) {
	t.Setenv("ORG_RATE_PER_SEC", "0") // 51 grants in a burst would trip the org's API rate first
	ts := newConnectorTestServer(t)
	ts.routesNode(t)
	ts.announceRoutes(t, "prod", `{"targets":[],"subnets":["10.0.0.0/8"]}`)
	for i := range sandbox.MaxEffectiveGrants {
		body := fmt.Sprintf(`{"connector":"prod","subnet":"10.%d.0.0/16"}`, i+1)
		if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, body); resp.StatusCode != http.StatusCreated {
			t.Fatalf("grant %d: %d %s", i, resp.StatusCode, ts.body(resp))
		}
	}
	resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.60.0.0/16"}`)
	if b := ts.body(resp); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(b, "at most") {
		t.Fatalf("over the limit: %d %s", resp.StatusCode, b)
	}
}

// A tag's subnet grant on a sandbox whose node cannot route it must not make
// a target grant to that sandbox fail: the routes are skipped, the target pushed.
func TestTargetGrantToOldNodeIgnoresTagRoutes(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true) // private_access only
	ts.announceRoutes(t, "prod", `{"targets":[{"name":"db","port":5432}],"subnets":["10.0.0.0/16"]}`)
	st := ts.s.st.Load()
	rec, _, err := st.SandboxByID(t.Context(), "sb1")
	if err != nil {
		t.Fatal(err)
	}
	rec.Networks = `["backend"]`
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGrant(t.Context(), store.Grant{OrgID: "org-a", ConnectorID: ts.connectorID(t, "prod"),
		SubjectKind: store.GrantSubjectTag, Subject: "backend", Kind: store.GrantKindSubnet, Value: "10.0.5.0/24"}); err != nil {
		t.Fatal(err)
	}
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("target on an old node with tag routes: %d %s", resp.StatusCode, ts.body(resp))
	}
	if got := node.lastPatch(t, "sb1"); !strings.Contains(got, `"name":"db"`) || strings.Contains(got, "private_routes") {
		t.Fatalf("node got %s", got)
	}
	if gs, _ := st.SandboxGrants(t.Context(), "sb1"); len(gs) != 1 {
		t.Fatalf("grants %+v", gs)
	}
}

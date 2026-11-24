package orchestrator

import (
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func (ts *connectorTestServer) nodeDialDestRaw(t *testing.T, key, sb, dest string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	q := url.Values{"sandbox": {sb}, "dest": {dest}}
	h := http.Header{}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.srv.URL, "http")+"/internal/private/dial?"+q.Encode(), h)
}

func (ts *connectorTestServer) nodeDialDest(t *testing.T, key, sb, dest string) net.Conn {
	t.Helper()
	ws, resp, err := ts.nodeDialDestRaw(t, key, sb, dest)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dest dial %s: %v %s", dest, err, status)
	}
	c := &wsConn{ws: ws}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (ts *connectorTestServer) nodeDialDestStatus(t *testing.T, key, sb, dest string) string {
	t.Helper()
	ws, resp, err := ts.nodeDialDestRaw(t, key, sb, dest)
	if err == nil {
		_ = ws.Close()
		return "101"
	}
	if resp == nil {
		t.Fatalf("dest dial: %v", err)
	}
	return strconv.Itoa(resp.StatusCode)
}

func echoes(t *testing.T, c net.Conn) {
	t.Helper()
	_, _ = c.Write([]byte("ping"))
	buf := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func nextOpen(t *testing.T, f *fakeInstance) map[string]any {
	t.Helper()
	select {
	case m := <-f.opens:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("the instance got no open")
		return nil
	}
}

func TestDestDialInsideASubnetGrant(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	f := ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.5.0/24"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d", resp.StatusCode)
	}
	c := ts.nodeDialDest(t, node.key, "sb1", "10.0.5.7:5432")
	echoes(t, c)
	if m := nextOpen(t, f); m["dest"] != "10.0.5.7:5432" || m["target"] != nil {
		t.Fatalf("open %v", m)
	}
	_ = c.Close()
	cid := ts.connectorID(t, "prod")
	waitFor(t, func() bool {
		rows, _ := ts.s.st.Load().PrivateConns(t.Context(), cid, 10)
		return len(rows) == 1 && rows[0].Result == "ok" && rows[0].Target == "10.0.5.0/24" && rows[0].Dest == "10.0.5.7:5432" && rows[0].EndedAt > 0
	})
}

func TestDestDialRefusals(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	other := ts.fakeNodeNamed(t, "node-b", true)
	ts.announceRoutes(t, "prod", routesCaps)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.0.0/16"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d", resp.StatusCode)
	}
	for _, c := range []struct{ key, sb, dest, want string }{
		{"", "sb1", "10.0.5.7:80", "401"},
		{other.key, "sb1", "10.0.5.7:80", "403"}, // node does not run sb1
		{node.key, "sb1", "10.1.0.1:80", "403"},  // outside every grant
		{node.key, "sb1", "[fd00::1]:80", "403"}, // IPv6 is not routed
		{node.key, "sb1", "10.0.5.7:0", "403"},   // no port
		{node.key, "sb1", "nonsense", "403"},
		{node.key, "sb-1-conn", "10.0.5.7:80", "403"}, // another sandbox, on no node
	} {
		if got := ts.nodeDialDestStatus(t, c.key, c.sb, c.dest); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

func TestAuthorizeDest(t *testing.T) {
	t.Parallel()
	s := &Server{}
	now := time.Now()
	gs := []store.Grant{
		{ConnectorID: "cA", Kind: store.GrantKindSubnet, Value: "10.0.0.0/8"},
		{ConnectorID: "cB", Kind: store.GrantKindDNS, Value: "corp.internal"},
	}
	addr := netip.MustParseAddr
	live := now.Add(time.Minute)
	s.privateDNS.learn("sb", addr("172.16.0.9"), dnsLearned{connectorID: "cB", instanceID: "i1", suffix: "corp.internal", name: "wiki.corp.internal", added: now, expires: live})
	s.privateDNS.learn("sb", addr("172.16.0.10"), dnsLearned{connectorID: "cB", suffix: "corp.internal", added: now, expires: now.Add(-time.Second)})
	s.privateDNS.learn("sb", addr("172.16.0.11"), dnsLearned{connectorID: "cA", suffix: "corp.internal", added: now, expires: live})
	s.privateDNS.learn("sb", addr("10.0.0.9"), dnsLearned{connectorID: "cB", suffix: "corp.internal", added: now, expires: live})
	for _, c := range []struct {
		sb, a, conn, value string
		ok                 bool
	}{
		{"sb", "10.0.5.7", "cA", "10.0.0.0/8", true},
		{"sb", "10.128.0.5", "", "", false},               // platform range inside the grant
		{"sb", "172.16.0.9", "cB", "corp.internal", true}, // live answer
		{"sb", "172.16.0.10", "", "", false},              // expired
		{"sb", "172.16.0.11", "", "", false},              // answered for a connector no longer granted the suffix
		{"sb", "10.0.0.9", "cA", "10.0.0.0/8", true},      // the subnet grant wins
		{"other", "172.16.0.9", "", "", false},            // another sandbox's answer
	} {
		g, ok := s.authorizeDest(gs, c.sb, addr(c.a), now)
		if ok != c.ok || g.connectorID != c.conn || g.value != c.value {
			t.Errorf("%s %s: %+v %v, want %s %s %v", c.sb, c.a, g, ok, c.conn, c.value, c.ok)
		}
	}
	if g, _ := s.authorizeDest(gs, "sb", addr("172.16.0.9"), now); g.instance != "i1" || g.name != "wiki.corp.internal" || g.subnet {
		t.Errorf("dns grant detail %+v", g)
	}
}

// Review focus 2: the hub records an answer before replying, so a dial made the
// moment the answer arrives is authorised.
func TestDNSAnswerAuthorisesTheNextDial(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	f := dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", routesCaps, true, wikiAnswers)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","dns":"corp.internal"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d", resp.StatusCode)
	}
	if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != http.StatusOK {
		t.Fatalf("lookup: %d", code)
	}
	echoes(t, ts.nodeDialDest(t, node.key, "sb1", "10.9.9.9:443"))
	if m := nextOpen(t, f); m["dest"] != "10.9.9.9:443" || m["name"] != "wiki.corp.internal" {
		t.Fatalf("open %v", m)
	}
	if got := ts.nodeDialDestStatus(t, node.key, "sb1", "10.9.9.8:443"); got != "403" {
		t.Fatalf("an address never answered: %s", got)
	}
}

// Review focus 5: a narrower re-announcement keeps the grant and fails closed.
func TestNarrowedAnnouncementFailsClosed(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.routesNode(t)
	dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", `{"targets":[],"subnets":["10.0.0.0/16"]}`, true, nil)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.0.0/16"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d", resp.StatusCode)
	}
	before := node.lastPatch(t, "sb1")
	dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", `{"targets":[],"subnets":["10.0.5.0/24"]}`, true, nil)
	cid := ts.connectorID(t, "prod")
	waitFor(t, func() bool {
		nets, _ := ts.s.connectors.routes(cid, nil, nil)
		return len(nets) == 1 && nets[0] == "10.0.5.0/24"
	})
	if got := ts.nodeDialDestStatus(t, node.key, "sb1", "10.0.5.7:80"); got != "101" {
		t.Fatalf("still announced: %s", got)
	}
	if got := ts.nodeDialDestStatus(t, node.key, "sb1", "10.0.9.9:80"); got != "502" {
		t.Fatalf("no longer announced: %s, want 502", got)
	}
	waitFor(t, func() bool {
		rows, _ := ts.s.st.Load().PrivateConns(t.Context(), cid, 10)
		for _, r := range rows {
			if r.Dest == "10.0.9.9:80" && r.Result == "offline" {
				return true
			}
		}
		return false
	})
	if gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1"); len(gs) != 1 {
		t.Fatalf("grant dropped: %+v", gs)
	}
	if got := node.lastPatch(t, "sb1"); got != before {
		t.Fatalf("a re-announcement re-pushed the sandbox, so the node would stop failing closed: %s", got)
	}
}

// Carry from Task 7: the answer cache outlives a revoke, so a dest dial
// re-checks the suffix grant on every open.
func TestRevokedDNSGrantRefusesCachedAddress(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.dnsGrant(t, wikiAnswers)
	if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != http.StatusOK {
		t.Fatalf("lookup: %d", code)
	}
	if got := ts.nodeDialDestStatus(t, node.key, "sb1", "10.9.9.9:443"); got != "101" {
		t.Fatalf("before revoke: %s", got)
	}
	gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1")
	if len(gs) != 1 {
		t.Fatalf("grants %+v", gs)
	}
	if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/"+gs[0].ID, ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if _, ok := ts.s.privateDNS.lookup("sb1", netip.MustParseAddr("10.9.9.9"), time.Now()); !ok {
		t.Fatal("the answer left the cache, so this test no longer covers the re-check")
	}
	if got := ts.nodeDialDestStatus(t, node.key, "sb1", "10.9.9.9:443"); got != "403" {
		t.Fatalf("after revoke: %s, want 403", got)
	}
}

// A dest connection is filed under its grant: another grant's change leaves it
// open, and revoking its own grant cuts it.
func TestRevokeClosesOpenDestConn(t *testing.T) {
	for _, c := range []struct{ name, kind, value, dest string }{
		{"subnet", "subnet", "10.0.0.0/16", "10.0.5.7:5432"},
		{"dns", "dns", "corp.internal", "10.9.9.9:443"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newConnectorTestServer(t)
			node := ts.routesNode(t)
			f := dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", routesCaps, true, wikiAnswers)
			if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","`+c.kind+`":"`+c.value+`"}`); resp.StatusCode != http.StatusCreated {
				t.Fatalf("grant: %d", resp.StatusCode)
			}
			if c.kind == "dns" {
				if code, _ := ts.nodeDNS(t, node.key, "sb1", "wiki.corp.internal", "A"); code != http.StatusOK {
					t.Fatalf("lookup: %d", code)
				}
			}
			conn := ts.nodeDialDest(t, node.key, "sb1", c.dest)
			echoes(t, conn)
			nextOpen(t, f)
			if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","subnet":"10.0.0.0/30"}`); resp.StatusCode != http.StatusCreated {
				t.Fatalf("second grant: %d %s", resp.StatusCode, ts.body(resp))
			}
			echoes(t, conn)
			gs, _ := ts.s.st.Load().SandboxGrants(t.Context(), "sb1")
			id := ""
			for _, g := range gs {
				if g.Value == c.value {
					id = g.ID
				}
			}
			if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/"+id, ts.key, ""); resp.StatusCode != http.StatusNoContent {
				t.Fatalf("revoke: %d", resp.StatusCode)
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("conn survived revoke")
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("conn not closed within 2s of revoke")
			}
		})
	}
}

func TestAuthorizeDestPrefersAnOnlineConnector(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.connectors.instances = map[string]map[string]*connectorInstance{
		"cB": {"i1": {instanceID: "i1", subnets: []netip.Prefix{netip.MustParsePrefix("10.0.5.0/24")}}},
	}
	gs := []store.Grant{
		{ConnectorID: "cA", Kind: store.GrantKindSubnet, Value: "10.0.0.0/8"},
		{ConnectorID: "cB", Kind: store.GrantKindSubnet, Value: "10.0.0.0/16"},
	}
	if g, ok := s.authorizeDest(gs, "sb", netip.MustParseAddr("10.0.5.7"), time.Now()); !ok || g.connectorID != "cB" {
		t.Fatalf("covered: %+v %v", g, ok)
	}
	if g, ok := s.authorizeDest(gs, "sb", netip.MustParseAddr("10.0.9.9"), time.Now()); !ok || g.connectorID != "cA" {
		t.Fatalf("no instance covers it, so the first grant: %+v %v", g, ok)
	}
}

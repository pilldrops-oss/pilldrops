package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

type fakeInstance struct {
	ctrl  *websocket.Conn
	opens chan map[string]any
}

func dialInstance(t *testing.T, ts *connectorTestServer, name, instance string, targets string, echo bool) *fakeInstance {
	t.Helper()
	return dialInstanceWith(t, ts, name, instance, `{"targets":`+targets+`}`, echo, nil)
}

// dialInstanceWith connects an instance whose hello announces caps. echo serves
// every open as an echo; dns, when set, answers relayed lookups with the body it
// returns, and a nil body leaves the lookup unanswered.
func dialInstanceWith(t *testing.T, ts *connectorTestServer, name, instance, caps string, echo bool, dns func(name, qtype string) map[string]any) *fakeInstance {
	t.Helper()
	tunnel, conn, tok := ts.session(t, name)
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })
	if err := ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"`+instance+`","version":"test","capabilities":`+caps+`}`)); err != nil {
		t.Fatal(err)
	}
	f := &fakeInstance{ctrl: ctrl, opens: make(chan map[string]any, 8)}
	go func() {
		for {
			var m map[string]any
			if err := ctrl.ReadJSON(&m); err != nil {
				close(f.opens)
				return
			}
			if m["type"] == "dns" && dns != nil {
				name, nameOK := m["name"].(string)
				qtype, qtypeOK := m["qtype"].(string)
				if !nameOK || !qtypeOK {
					t.Errorf("dns frame has name %v and qtype %v, want strings", m["name"], m["qtype"])
					return
				}
				if reply := dns(name, qtype); reply != nil {
					reply["type"], reply["id"] = "dns_answer", m["id"]
					_ = ctrl.WriteJSON(reply)
				}
				continue
			}
			if m["type"] != "open" {
				continue
			}
			select {
			case f.opens <- m:
			default: // nobody reads past the buffer; never stall the instance
			}
			id, idOK := m["id"].(string)
			if !idOK {
				t.Errorf("open frame has id %v, want a string", m["id"])
				return
			}
			if !echo {
				_ = ctrl.WriteJSON(map[string]any{"type": "fail", "id": id, "reason": "dial"})
				continue
			}
			go func() {
				ws, _, err := websocket.DefaultDialer.Dial(conn+id, nil)
				if err != nil {
					return
				}
				c := &wsConn{ws: ws}
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	waitFor(t, func() bool { return ts.s.connectors.count(ts.connectorID(t, name)) > 0 })
	return f
}

func TestHubOpensAndSplices(t *testing.T) {
	ts := newConnectorTestServer(t)
	dialInstance(t, ts, "prod", "inst-aaaaaaaaaaaa", `[{"name":"db","port":5432}]`, true)
	id := ts.connectorID(t, "prod")
	c, result, err := ts.s.openPrivate(context.Background(), id, "db", 5432)
	if err != nil {
		t.Fatalf("open: %v %s", err, result)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestHubFailsOverToSecondInstance(t *testing.T) {
	ts := newConnectorTestServer(t)
	bad := dialInstance(t, ts, "prod", "inst-bad-aaaaaaaa", `[{"name":"db","port":5432}]`, false)
	dialInstance(t, ts, "prod", "inst-good-aaaaaaa", `[{"name":"db","port":5432}]`, true)
	id := ts.connectorID(t, "prod")
	for range 4 {
		c, result, err := ts.s.openPrivate(context.Background(), id, "db", 5432)
		if err != nil {
			t.Fatalf("open: %v %s", err, result)
		}
		_ = c.Close()
	}
	_ = bad
}

func TestHubOfflineAndUnknownTarget(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	id := ts.connectorID(t, "prod")
	if _, result, err := ts.s.openPrivate(context.Background(), id, "db", 5432); err == nil || result != "offline" {
		t.Fatalf("no instance: %v %q", err, result)
	}
	dialInstance(t, ts, "prod", "inst-aaaaaaaaaaaa", `[{"name":"db","port":5432}]`, true)
	if _, result, err := ts.s.openPrivate(context.Background(), id, "api", 443); err == nil || result != "offline" {
		t.Fatalf("unannounced target: %v %q", err, result)
	}
	if _, _, err := ts.s.openPrivate(context.Background(), id, "db", 5433); err == nil {
		t.Fatal("wrong port opened")
	}
}

func TestHubRefusesPlatformHostname(t *testing.T) {
	t.Setenv("PORTAL_ORIGIN", "https://pilldrops.com")
	ts := newConnectorTestServer(t)
	ts.s.portalOrigin = "" // the session URLs must stay on the test server
	tunnel, _, tok := ts.session(t, "prod")
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctrl.Close() }()
	_ = ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"inst-aaaaaaaaaaaa","capabilities":{"targets":[{"name":"api","port":443,"host":"api.pilldrops.com"}]}}`))
	var m map[string]any
	if err := ctrl.ReadJSON(&m); err != nil || m["type"] != "closed" || !strings.Contains(m["reason"].(string), "platform") {
		t.Fatalf("got %v %v", m, err)
	}
}

func TestHubInstanceDropEndsConn(t *testing.T) {
	ts := newConnectorTestServer(t)
	f := dialInstance(t, ts, "prod", "inst-aaaaaaaaaaaa", `[{"name":"db","port":5432}]`, true)
	id := ts.connectorID(t, "prod")
	c, _, err := ts.s.openPrivate(context.Background(), id, "db", 5432)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.ctrl.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("conn outlived its instance")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("conn hung instead of closing")
	}
}

func (ts *connectorTestServer) session(t *testing.T, name string) (tunnelURL, connURL, token string) {
	t.Helper()
	return ts.sessionAs(t, ts.key, name)
}

func (ts *connectorTestServer) sessionAs(t *testing.T, key, name string) (tunnelURL, connURL, token string) {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(ts.body(ts.do(t, "POST", "/v1/connectors/session", key, `{"name":"`+name+`"}`))), &m); err != nil {
		t.Fatal(err)
	}
	return m["tunnel_url"], m["conn_url"], m["tunnel_token"]
}

func TestAbandonedPendingClosesALateDataSocket(t *testing.T) {
	var h connectorHub
	ch := make(chan net.Conn, 1)
	id := h.addPending(ch)
	h.takePending(id) // the data handler got there first
	h.abandonPending(id, ch)
	server, client := net.Pipe()
	ch <- server
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late socket not closed: %v", err)
	}
}

// keyInstance connects an instance through a session opened with a connector key.
func keyInstance(t *testing.T, ts *connectorTestServer, key, instance string) (ctrl *websocket.Conn, tunnel, token string) {
	t.Helper()
	tunnel, _, token = ts.sessionAs(t, key, "prod")
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })
	if err := ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"`+instance+`","capabilities":{"targets":[]}}`)); err != nil {
		t.Fatal(err)
	}
	return ctrl, tunnel, token
}

func TestKeyRevokeEndsItsInstances(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	id := ts.connectorID(t, "prod")
	newKey := func() (keyID, plain string) {
		var k struct{ ID, Key string }
		if err := json.Unmarshal([]byte(ts.body(ts.do(t, "POST", "/v1/connectors/"+id+"/keys", ts.key, ""))), &k); err != nil {
			t.Fatal(err)
		}
		return k.ID, k.Key
	}
	revokedID, revoked := newKey()
	_, kept := newKey()
	gone, tunnel, token := keyInstance(t, ts, revoked, "inst-revoked-aaaa")
	keyInstance(t, ts, kept, "inst-kept-aaaaaaa")
	dialInstance(t, ts, "prod", "inst-apikey-aaaaa", `[]`, true)
	waitFor(t, func() bool { return ts.s.connectors.count(id) == 3 })

	if resp := ts.do(t, "DELETE", "/v1/connectors/"+id+"/keys/"+revokedID, ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke key: %d", resp.StatusCode)
	}
	_ = gone.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m map[string]any
	if err := gone.ReadJSON(&m); err != nil || m["type"] != "closed" || m["reason"] != "key revoked" {
		t.Fatalf("revoked key's instance got %v %v", m, err)
	}
	waitFor(t, func() bool { return ts.s.connectors.count(id) == 2 })

	_, resp, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + token}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("token for a revoked key: %v %v", resp, err)
	}
	if _, ok := ts.s.validConnectorToken(token[strings.IndexByte(token, '.')+1:], id); ok {
		t.Fatal("a key session's token without its key id passed as an API-key session")
	}
}

func TestTunnelStoreErrorIsRetryable(t *testing.T) {
	ts := newConnectorTestServer(t)
	tunnel, _, token := ts.session(t, "prod")
	_ = ts.s.st.Load().Close()
	_, resp, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + token}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("tunnel on a store error: %v %v", resp, err)
	}
}

// Both must arrive: the ping keeps the hub's read deadline fresh, and the
// keepalive is what the connector sees when a proxy in between eats the ping.
func TestHubPingsAConnectedInstance(t *testing.T) {
	connectorPingEvery = 50 * time.Millisecond
	t.Cleanup(func() { connectorPingEvery = 30 * time.Second })
	ts := newConnectorTestServer(t)
	tunnel, _, tok := ts.session(t, "prod")
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctrl.Close() }()
	if err := ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"inst-ping-aaaaaaaa","capabilities":{"targets":[]}}`)); err != nil {
		t.Fatal(err)
	}
	pinged := make(chan struct{}, 1)
	ctrl.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})
	kept := make(chan struct{}, 1)
	go func() {
		for {
			var m map[string]any
			if err := ctrl.ReadJSON(&m); err != nil {
				return
			}
			if m["type"] == "keepalive" {
				select {
				case kept <- struct{}{}:
				default:
				}
			}
		}
	}()
	for name, ch := range map[string]chan struct{}{"ping": pinged, "keepalive": kept} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s reached the connected instance", name)
		}
	}
}

// A stalled instance must not hold up ending the others: each end can wait
// out a 10 s send deadline.
func TestCloseConnectorEndsInstancesAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := shareUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	var h connectorHub
	var ins []*connectorInstance
	for i := range maxConnectorInstances {
		ctrl, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ctrl.Close() })
		in := &connectorInstance{connectorID: "cn", instanceID: fmt.Sprint("inst-", i), ctrl: ctrl, done: make(chan struct{})}
		if _, ok := h.add(in); !ok {
			t.Fatal("add")
		}
		in.writeMu.Lock() // a send stuck on a stalled socket
		ins = append(ins, in)
	}
	closed := make(chan struct{})
	go func() { h.closeConnector("cn", "revoked"); close(closed) }()
	// Freed in reverse: one at a time, only ending them concurrently lets each finish in turn.
	for i := len(ins) - 1; i >= 0; i-- {
		ins[i].writeMu.Unlock()
		select {
		case <-ins[i].done:
		case <-time.After(2 * time.Second):
			t.Fatalf("instance %d waited on the others", i)
		}
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closeConnector did not return")
	}
}

// A key revoked on another replica never reaches this one's hub: the periodic
// re-check is what ends its instance.
func TestHubRecheckEndsAnInstanceWhoseKeyWasRevoked(t *testing.T) {
	connectorPingEvery = 50 * time.Millisecond
	t.Cleanup(func() { connectorPingEvery = 30 * time.Second })
	ts := newConnectorTestServer(t)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	id := ts.connectorID(t, "prod")
	var k struct{ ID, Key string }
	if err := json.Unmarshal([]byte(ts.body(ts.do(t, "POST", "/v1/connectors/"+id+"/keys", ts.key, ""))), &k); err != nil {
		t.Fatal(err)
	}
	ctrl, _, _ := keyInstance(t, ts, k.Key, "inst-recheck-aaaa")
	waitFor(t, func() bool { return ts.s.connectors.count(id) == 1 })
	if ok, err := ts.s.st.Load().RevokeConnectorKey(t.Context(), id, k.ID, ts.s.now().Unix()); err != nil || !ok {
		t.Fatalf("revoke in store: %v %v", ok, err)
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var m map[string]any
		if err := ctrl.ReadJSON(&m); err != nil {
			t.Fatalf("instance not ended: %v", err)
		}
		if m["type"] == "closed" {
			if m["reason"] != "key revoked" {
				t.Fatalf("closed for %v", m["reason"])
			}
			break
		}
	}
	waitFor(t, func() bool { return ts.s.connectors.count(id) == 0 })
}

func TestHubRefusesAnInstanceOverTheLimit(t *testing.T) {
	ts := newConnectorTestServer(t)
	for i := range maxConnectorInstances {
		dialInstance(t, ts, "prod", fmt.Sprintf("inst-limit-%06d", i), `[]`, true)
	}
	tunnel, _, tok := ts.session(t, "prod")
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctrl.Close() }()
	_ = ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"inst-limit-over00","capabilities":{"targets":[]}}`))
	var m map[string]any
	_ = ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ctrl.ReadJSON(&m); err != nil || m["type"] != "closed" || !strings.Contains(m["reason"].(string), "maximum") {
		t.Fatalf("instance over the limit got %v %v", m, err)
	}
	if n := ts.s.connectors.count(ts.connectorID(t, "prod")); n != maxConnectorInstances {
		t.Fatalf("%d instances, want %d", n, maxConnectorInstances)
	}
	// A reconnect under a known instance id replaces it rather than counting against the limit.
	dialInstance(t, ts, "prod", "inst-limit-000000", `[]`, true)
}

func TestDataSocketIDIsSingleUse(t *testing.T) {
	ts := newConnectorTestServer(t)
	ch := make(chan net.Conn, 1)
	id := ts.s.connectors.addPending(ch)
	connURL := "ws" + strings.TrimPrefix(ts.srv.URL, "http") + "/v1/connector-conns/" + id
	ws, _, err := websocket.DefaultDialer.Dial(connURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if c := <-ch; c == nil {
		t.Fatal("first use got no socket")
	} else {
		defer func() { _ = c.Close() }()
	}
	if _, resp, err := websocket.DefaultDialer.Dial(connURL, nil); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second use of one id: %v %v", resp, err)
	}
}

func TestConnectorCounterReleasedAfterClose(t *testing.T) {
	ts := newConnectorTestServer(t)
	dialInstance(t, ts, "prod", "inst-aaaaaaaaaaaa", `[{"name":"db","port":5432}]`, true)
	id := ts.connectorID(t, "prod")
	active := ts.s.connectors.counter(id)
	c, result, err := ts.s.openPrivate(context.Background(), id, "db", 5432)
	if err != nil {
		t.Fatalf("open: %v %s", err, result)
	}
	if n := active.Load(); n != 1 {
		t.Fatalf("open conns %d, want 1", n)
	}
	_ = c.Close()
	_ = c.Close()
	if n := active.Load(); n != 0 {
		t.Fatalf("open conns after close %d, want 0", n)
	}
	if _, _, err := ts.s.openPrivate(context.Background(), id, "api", 443); err == nil {
		t.Fatal("unannounced target opened")
	}
	if n := active.Load(); n != 0 {
		t.Fatalf("open conns after a failed open %d, want 0", n)
	}
}

func TestHubStoresAnnouncedSubnetsAndSuffixes(t *testing.T) {
	ts := newConnectorTestServer(t)
	dialInstanceWith(t, ts, "prod", "inst-aaaaaaaaaaaa", `{"targets":[],"subnets":["10.0.0.0/16"],"dns":{"suffixes":["Corp.Internal."]}}`, true, nil)
	var out struct {
		Connectors []struct {
			Subnets []string
			DNS     struct{ Suffixes []string }
		}
	}
	if err := json.Unmarshal([]byte(ts.body(ts.do(t, "GET", "/v1/connectors", ts.key, ""))), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Connectors) != 1 || !slices.Equal(out.Connectors[0].Subnets, []string{"10.0.0.0/16"}) ||
		!slices.Equal(out.Connectors[0].DNS.Suffixes, []string{"corp.internal"}) {
		t.Fatalf("listed %+v", out)
	}
	c, _, err := ts.s.st.Load().ConnectorByID(t.Context(), "", ts.connectorID(t, "prod"))
	if err != nil || !slices.Equal(c.Subnets, []string{"10.0.0.0/16"}) || !slices.Equal(c.Suffixes, []string{"corp.internal"}) {
		t.Fatalf("stored %+v %v", c, err)
	}
}

func TestHubRefusesBadAnnouncements(t *testing.T) {
	t.Setenv("PORTAL_ORIGIN", "https://pilldrops.com")
	t.Setenv("SHARE_DOMAIN", "share.example.org")
	ts := newConnectorTestServer(t)
	ts.s.portalOrigin = "" // the session URLs must stay on the test server
	many := make([]string, sandbox.MaxConnectorSubnets+1)
	for i := range many {
		many[i] = fmt.Sprintf("10.%d.0.0/16", i)
	}
	manyJSON, _ := json.Marshal(many)
	manySuffixes := make([]string, sandbox.MaxConnectorSuffixes+1)
	for i := range manySuffixes {
		manySuffixes[i] = fmt.Sprintf("s%d.corp.internal", i)
	}
	manySuffixesJSON, _ := json.Marshal(manySuffixes)
	for _, c := range []struct{ caps, reason string }{
		{`{"subnets":["0.0.0.0/0"]}`, "every address"},
		{`{"subnets":["10.0.5.1/24"]}`, "host bits"},
		{`{"subnets":` + string(manyJSON) + `}`, "at most 20"},
		{`{"dns":{"suffixes":["wiki.pilldrops.com"]}}`, "platform"},
		{`{"dns":{"suffixes":["co.uk"]}}`, "public suffix"},
		{`{"dns":{"suffixes":["example.org"]}}`, "capture the platform domain"},
		{`{"dns":{"suffixes":` + string(manySuffixesJSON) + `}}`, "at most 10"},
		{`{"dns":{"suffixes":["10.0.0.1"]}}`, "IP address"},
		{`{"dns":{"suffixes":["x.pd"]}}`, "sandbox-internal"},
		{`{"dns":{"suffixes":["internal"]}}`, "single label"},
	} {
		tunnel, _, tok := ts.session(t, "prod")
		ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
		if err != nil {
			t.Fatal(err)
		}
		_ = ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"inst-aaaaaaaaaaaa","capabilities":`+c.caps+`}`))
		var m map[string]any
		err = ctrl.ReadJSON(&m)
		_ = ctrl.Close()
		if reason, _ := m["reason"].(string); err != nil || m["type"] != "closed" || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: got %v %v, want closed with %q", c.caps, m, err, c.reason)
		}
	}
}

func TestInstanceCoversAndServes(t *testing.T) {
	t.Parallel()
	in := &connectorInstance{subnets: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}, suffixes: []string{"corp.internal"}}
	if !in.coversAddr(netip.MustParseAddr("10.0.5.7")) || in.coversAddr(netip.MustParseAddr("10.1.0.1")) {
		t.Error("coversAddr")
	}
	if !in.servesSuffix("wiki.corp.internal") || !in.servesSuffix("corp.internal") || in.servesSuffix("internal") {
		t.Error("servesSuffix")
	}
}

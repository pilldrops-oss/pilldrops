package orchestrator

import (
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPrivateConnSetCloseFor(t *testing.T) {
	var set privateConnSet
	closed := map[string]bool{}
	for _, c := range []*privateConn{{sandboxID: "a", connectorID: "c1", target: "db"}, {sandboxID: "a", connectorID: "c2", target: "db"}, {sandboxID: "a", connectorID: "c1", target: "api"}, {sandboxID: "b", connectorID: "c1", target: "db"}} {
		name := c.sandboxID + "/" + c.connectorID + "/" + c.target
		c.close = func() { closed[name] = true }
		set.add(c)
	}
	set.closeFor("a", "c1", "db")
	if !closed["a/c1/db"] || closed["a/c2/db"] || closed["a/c1/api"] || closed["b/c1/db"] {
		t.Fatalf("closeFor(a, c1, db) closed %v", closed)
	}
	set.closeFor("a", "", "")
	if !closed["a/c1/api"] || !closed["a/c2/db"] || closed["b/c1/db"] {
		t.Fatalf("closeFor(a, all) closed %v", closed)
	}
	gone := &privateConn{sandboxID: "c", close: func() { t.Fatal("removed conn closed") }}
	set.add(gone)
	set.remove(gone)
	set.closeFor("c", "", "")
}

func (ts *connectorTestServer) nodeDialRaw(t *testing.T, key, sb, target string, port int) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	q := url.Values{"sandbox": {sb}, "target": {target}, "port": {strconv.Itoa(port)}}
	h := http.Header{}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.srv.URL, "http")+"/internal/private/dial?"+q.Encode(), h)
}

func (ts *connectorTestServer) nodeDial(t *testing.T, key, sb, target string, port int) net.Conn {
	t.Helper()
	ws, resp, err := ts.nodeDialRaw(t, key, sb, target, port)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("node dial: %v %s", err, status)
	}
	c := &wsConn{ws: ws}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// nodeDialStatus returns the handshake's HTTP status, "101" when it upgraded.
func (ts *connectorTestServer) nodeDialStatus(t *testing.T, key, sb, target string, port int) string {
	t.Helper()
	ws, resp, err := ts.nodeDialRaw(t, key, sb, target, port)
	if err == nil {
		_ = ws.Close()
		return "101"
	}
	if resp == nil {
		t.Fatalf("node dial: %v", err)
	}
	return strconv.Itoa(resp.StatusCode)
}

func (ts *connectorTestServer) grantDB(t *testing.T) {
	t.Helper()
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	ts.announce(t, "prod", `[{"name":"db","port":5432},{"name":"api","port":443}]`)
	if resp := ts.do(t, "POST", "/v1/sandboxes/sb1/private", ts.key, `{"connector":"prod","target":"db"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d %s", resp.StatusCode, ts.body(resp))
	}
}

func TestPrivateDialEndToEnd(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	c := ts.nodeDial(t, node.key, "sb1", "db", 5432)
	_, _ = c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("%q %v", buf, err)
	}
	_ = c.Close()
	cid := ts.connectorID(t, "prod")
	waitFor(t, func() bool {
		rows, _ := ts.s.st.Load().PrivateConns(t.Context(), cid, 10)
		return len(rows) == 1 && rows[0].Result == "ok" && rows[0].BytesIn == 4 && rows[0].BytesOut == 4 &&
			rows[0].EndedAt > 0 && rows[0].NodeID == node.id && rows[0].OrgID == "org-a"
	})
}

func TestPrivateDialRefusals(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	other := ts.fakeNodeNamed(t, "node-b", true)
	ts.grantDB(t)
	for _, c := range []struct {
		key, sb, target string
		port            int
		want            string
	}{
		{"", "sb1", "db", 5432, "401"},
		{"not-a-key", "sb1", "db", 5432, "401"},
		{ts.s.apiKey, "sb1", "db", 5432, "403"}, // operator key is not a node
		{ts.sandboxToken, "sb1", "db", 5432, "401"},
		{other.key, "sb1", "db", 5432, "403"},    // node does not run sb1
		{node.key, "sb-none", "db", 5432, "403"}, // no such sandbox
		{node.key, "sb1", "api", 443, "403"},     // not granted
		{node.key, "sb1", "db", 5433, "403"},     // wrong port
	} {
		if got := ts.nodeDialStatus(t, c.key, c.sb, c.target, c.port); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
	rows, _ := ts.s.st.Load().PrivateConns(t.Context(), ts.connectorID(t, "prod"), 10)
	if len(rows) != 1 || rows[0].Result != "denied" || rows[0].Target != "db" {
		t.Fatalf("want 1 denied row under prod (wrong port), got %+v", rows)
	}
	// The ungranted target has no connector to file under.
	rows, _ = ts.s.st.Load().PrivateConns(t.Context(), "", 10)
	if len(rows) != 1 || rows[0].Result != "denied" || rows[0].Target != "api" {
		t.Fatalf("want 1 denied row with no connector (ungranted), got %+v", rows)
	}
}

func TestPrivateDialRemovedNode(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	if err := ts.s.st.Load().MarkNodeRemoved(t.Context(), node.id, "", 1); err != nil {
		t.Fatal(err)
	}
	if got := ts.nodeDialStatus(t, node.key, "sb1", "db", 5432); got != "403" {
		t.Fatalf("removed node: %s", got)
	}
}

func TestRevokeClosesOpenPrivateConn(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	c := ts.nodeDial(t, node.key, "sb1", "db", 5432)
	ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/prod/db", ts.key, "")
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("conn survived revoke")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("conn not closed within 2s of revoke")
	}
}

// A revoke whose push fails after the grant is gone still cuts the connection.
func TestRevokeClosesConnWhenThePushFails(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	c := ts.nodeDial(t, node.key, "sb1", "db", 5432)
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_private BEFORE UPDATE ON sandboxes BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if resp := ts.do(t, "DELETE", "/v1/sandboxes/sb1/private/prod/db", ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", resp.StatusCode, ts.body(resp))
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("conn survived revoke")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("conn not closed within 2s of revoke")
	}
}

func TestPrivateDialPerSandboxLimit(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	var open []net.Conn
	for range maxPrivateConnsPerSandbox {
		open = append(open, ts.nodeDial(t, node.key, "sb1", "db", 5432))
		time.Sleep(25 * time.Millisecond) // stay under the opens-per-second limit
	}
	if got := ts.nodeDialStatus(t, node.key, "sb1", "db", 5432); got != "429" {
		t.Fatalf("over the concurrent limit: %s", got)
	}
	for _, c := range open {
		_ = c.Close()
	}
	cid := ts.connectorID(t, "prod")
	waitFor(t, func() bool {
		rows, _ := ts.s.st.Load().PrivateConns(t.Context(), cid, 200)
		return len(rows) == maxPrivateConnsPerSandbox+1
	})
}

func TestPrivateConnSetOpensPerSecond(t *testing.T) {
	var set privateConnSet
	for range privateOpensPerSecond {
		if !set.allow("a") {
			t.Fatal("refused within the burst")
		}
	}
	if set.allow("a") {
		t.Fatal("allowed past the opens-per-second limit")
	}
	if !set.allow("b") {
		t.Fatal("one sandbox's rate applied to another")
	}
}

func TestPrivateConnSetLimits(t *testing.T) {
	var set privateConnSet
	var held []*privateConn
	for range maxPrivateConnsPerSandbox {
		c := &privateConn{sandboxID: "a", close: func() {}}
		if !set.add(c) {
			t.Fatal("refused under the limit")
		}
		held = append(held, c)
	}
	if set.add(&privateConn{sandboxID: "a", close: func() {}}) {
		t.Fatal("accepted over the limit")
	}
	if !set.add(&privateConn{sandboxID: "b", close: func() {}}) {
		t.Fatal("one sandbox's limit applied to another")
	}
	set.remove(held[0])
	set.remove(held[0]) // a second remove must not free another slot
	if !set.add(&privateConn{sandboxID: "a", close: func() {}}) {
		t.Fatal("a freed slot was not reusable")
	}
	if set.add(&privateConn{sandboxID: "a", close: func() {}}) {
		t.Fatal("double remove freed two slots")
	}
}

func TestPrivateSlotCutBeforeUpstream(t *testing.T) {
	s := &privateSlot{}
	s.close()
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	if s.set(a) {
		t.Fatal("a cut slot accepted its upstream")
	}
	s2 := &privateSlot{}
	if !s2.set(a) {
		t.Fatal("a live slot refused its upstream")
	}
	s2.close()
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("close did not close the upstream")
	}
}

func TestPrivateDialOpensPerSecondAnswers429(t *testing.T) {
	prev := privateOpensPerSecond
	privateOpensPerSecond = 1
	t.Cleanup(func() { privateOpensPerSecond = prev })
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	ts.grantDB(t)
	_ = ts.nodeDial(t, node.key, "sb1", "db", 5432).Close()
	if got := ts.nodeDialStatus(t, node.key, "sb1", "db", 5432); got != "429" {
		t.Fatalf("second open within the second: %s", got)
	}
	cid := ts.connectorID(t, "prod")
	waitFor(t, func() bool {
		rows, _ := ts.s.st.Load().PrivateConns(t.Context(), cid, 10)
		return len(rows) == 1 && rows[0].Result == "ok"
	})
	// The limit comes before the sandbox lookup: an unknown sandbox gets 429, not 403, once over it.
	if got := ts.nodeDialStatus(t, node.key, "sb-none", "db", 5432); got != "403" {
		t.Fatalf("first open for an unknown sandbox: %s", got)
	}
	if got := ts.nodeDialStatus(t, node.key, "sb-none", "db", 5432); got != "429" {
		t.Fatalf("second open for an unknown sandbox: %s", got)
	}
}

func TestPrivateConnSetRefuseWarnsOncePerMinute(t *testing.T) {
	var set privateConnSet
	t0 := time.Unix(1000, 0)
	if n := set.refuse("a", t0); n != 1 {
		t.Fatalf("first refusal: %d, want 1", n)
	}
	for i := 1; i <= 3; i++ {
		if n := set.refuse("a", t0.Add(time.Duration(i)*time.Second)); n != 0 {
			t.Fatalf("refusal within the minute warned: %d", n)
		}
	}
	if n := set.refuse("b", t0); n != 1 {
		t.Fatalf("another sandbox shares the warning: %d", n)
	}
	if n := set.refuse("a", t0.Add(time.Minute)); n != 4 {
		t.Fatalf("after a minute: %d, want the 4 refusals since the last warning", n)
	}
}

func TestPrivateConnSetSweepsIdleLimiters(t *testing.T) {
	var set privateConnSet
	held := &privateConn{sandboxID: "busy", close: func() {}}
	set.add(held)
	set.allow("busy")
	set.allow("idle")
	time.Sleep(50 * time.Millisecond) // both buckets refill
	set.mu.Lock()
	set.swept = time.Time{}
	set.mu.Unlock()
	set.allow("other")
	set.mu.Lock()
	defer set.mu.Unlock()
	if _, ok := set.limits["idle"]; ok {
		t.Fatal("an idle, refilled limiter was kept")
	}
	if _, ok := set.limits["busy"]; !ok {
		t.Fatal("a limiter with an open connection was dropped")
	}
}

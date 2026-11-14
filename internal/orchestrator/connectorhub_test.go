package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeInstance struct {
	ctrl  *websocket.Conn
	opens chan map[string]any
}

func dialInstance(t *testing.T, ts *connectorTestServer, name, instance string, targets string, echo bool) *fakeInstance {
	t.Helper()
	tunnel, conn, tok := ts.session(t, name)
	ctrl, _, err := websocket.DefaultDialer.Dial(tunnel, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })
	if err := ctrl.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello","instance_id":"`+instance+`","version":"test","capabilities":{"targets":`+targets+`}}`)); err != nil {
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
			if m["type"] != "open" {
				continue
			}
			select {
			case f.opens <- m:
			default: // nobody reads past the buffer; never stall the instance
			}
			id := m["id"].(string)
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
	var m map[string]string
	if err := json.Unmarshal([]byte(ts.body(ts.do(t, "POST", "/v1/connectors/session", ts.key, `{"name":"`+name+`"}`))), &m); err != nil {
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

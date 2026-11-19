package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// The connector API and its sockets reach the orchestrator through the portal;
// the sockets and a pd_conn_ session keep their own credential.
func TestPortalForwardsConnectorRoutesAndSockets(t *testing.T) {
	seen := make(chan *http.Request, 8)
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		if r.URL.Path == "/v1/connectors/cn1/tunnel" {
			ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = ws.Close() }()
			if _, msg, err := ws.ReadMessage(); err == nil {
				_ = ws.WriteMessage(websocket.TextMessage, msg)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer orch.Close()

	r := chi.NewRouter()
	passthrough := func(w http.ResponseWriter, req *http.Request) {
		body, status, _ := proxyToOrchestrator(req, orch.URL, "portal-key", "u:a")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	registerFleetPassthrough(r, passthrough)
	authed := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	registerConnectorRoutes(r, connectorProxy(orch.URL), authed)
	portal := httptest.NewServer(r)
	defer portal.Close()

	call := func(method, path, bearer string) *http.Response {
		req, _ := http.NewRequest(method, portal.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/connectors"}, {"POST", "/v1/connectors"}, {"DELETE", "/v1/connectors/cn1"},
		{"GET", "/v1/connectors/cn1/connections"}, {"POST", "/v1/connectors/cn1/keys"}, {"DELETE", "/v1/connectors/cn1/keys/ck1"},
	} {
		if resp := call(c.method, c.path, "pd_customer"); resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: %d", c.method, c.path, resp.StatusCode)
			continue
		}
		if got := <-seen; got.URL.Path != c.path || got.Header.Get("Authorization") != "Bearer portal-key" {
			t.Errorf("%s %s reached the orchestrator as %s with %q", c.method, c.path, got.URL.Path, got.Header.Get("Authorization"))
		}
	}

	if resp := call("POST", "/v1/connectors/session", "pd_customer"); resp.StatusCode != http.StatusTeapot {
		t.Errorf("an API-key session skipped the portal's auth: %d", resp.StatusCode)
	}
	if resp := call("POST", "/v1/connectors/session", "pd_conn_abc"); resp.StatusCode != http.StatusOK {
		t.Errorf("connector-key session: %d", resp.StatusCode)
	} else if got := <-seen; got.Header.Get("Authorization") != "Bearer pd_conn_abc" {
		t.Errorf("connector key not passed through: %q", got.Header.Get("Authorization"))
	}

	wsURL := "ws" + strings.TrimPrefix(portal.URL, "http") + "/v1/connectors/cn1/tunnel"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer tunnel-tok"}})
	if err != nil {
		t.Fatalf("tunnel upgrade through the portal: %v", err)
	}
	defer func() { _ = ws.Close() }()
	got := <-seen
	if got.Header.Get("Authorization") != "Bearer tunnel-tok" || got.Header.Get("X-Forwarded-Host") != strings.TrimPrefix(portal.URL, "http://") {
		t.Errorf("tunnel reached the orchestrator with %q, forwarded host %q", got.Header.Get("Authorization"), got.Header.Get("X-Forwarded-Host"))
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "hello" {
		t.Fatalf("echo through the portal: %q %v", msg, err)
	}
}

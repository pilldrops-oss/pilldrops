package nodeboot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

func TestPrivateDialerSendsNodeKeyAndSplices(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/private/dial" || r.Header.Get("Authorization") != "Bearer sf_node_x" ||
			r.URL.Query().Get("sandbox") != "sb1" || r.URL.Query().Get("target") != "db" || r.URL.Query().Get("port") != "5432" {
			http.Error(w, "bad", http.StatusForbidden)
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, msg, _ := ws.ReadMessage()
		_ = ws.WriteMessage(websocket.BinaryMessage, msg)
	}))
	defer srv.Close()
	dial := NewPrivateDialer(srv.URL, "sf_node_x")
	c, err := dial(context.Background(), "sb1", "db", 5432)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("%q %v", buf, err)
	}
	if _, err := dial(context.Background(), "sb2", "db", 5432); err == nil {
		t.Fatal("refused dial returned a conn")
	}
}

func TestPrivateDialBase(t *testing.T) {
	for in, want := range map[string]string{
		"http://orch:8080/":         "ws://orch:8080",
		"https://api.example.com":   "wss://api.example.com",
		"https://example.com/base/": "wss://example.com/base",
		"orch:8080":                 "",
		"ftp://orch":                "",
		"":                          "",
	} {
		got, err := privateDialBase(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := NewPrivateDialer("orch:8080", "k")(context.Background(), "sb", "db", 1); err == nil {
		t.Error("a dialer built from a schemeless URL dialled")
	}
}

// The dial route takes only a node's own key, so the operator key must not switch it on.
func TestPrivateDialerUsesTheNodeKey(t *testing.T) {
	t.Setenv("ORCHESTRATOR_URL", "https://orch.example")
	t.Setenv("ORCHESTRATOR_API_KEY", "operator-key")
	t.Setenv("NODE_API_KEY", "")
	if privateDialer() != nil {
		t.Fatal("dialer built from the operator key")
	}
	t.Setenv("NODE_API_KEY", "sf_node_k")
	if privateDialer() == nil {
		t.Fatal("no dialer with a node key")
	}
}

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

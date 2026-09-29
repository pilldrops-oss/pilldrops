package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// mockGateway speaks just enough Discord Gateway: HELLO, expect IDENTIFY, deliver
// scripted dispatches, record heartbeats.
func mockGateway(t *testing.T, dispatches []payload, gotIdentify chan<- json.RawMessage, gotHeartbeat chan<- struct{}) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		hello, _ := json.Marshal(map[string]int{"heartbeat_interval": 50})
		_ = conn.WriteJSON(payload{Op: opHello, D: hello})

		var id payload
		if err := conn.ReadJSON(&id); err != nil || id.Op != opIdentify {
			t.Errorf("first frame op = %d (err %v), want IDENTIFY", id.Op, err)
			return
		}
		gotIdentify <- id.D

		for _, d := range dispatches {
			_ = conn.WriteJSON(d)
		}
		for {
			var p payload
			if err := conn.ReadJSON(&p); err != nil {
				return
			}
			if p.Op == opHeartbeat {
				select {
				case gotHeartbeat <- struct{}{}:
				default:
				}
			}
		}
	}))
}

func dispatch(t *testing.T, event string, v any) payload {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return payload{Op: opDispatch, T: event, D: raw}
}

func TestGatewayIdentifiesHeartbeatsAndDelivers(t *testing.T) {
	me := map[string]any{
		"id": "m1", "channel_id": "c1", "content": "hello agent",
		"author": map[string]any{"id": "user1", "bot": false},
	}
	fromBot := map[string]any{
		"id": "m2", "channel_id": "c1", "content": "echo from a bot",
		"author": map[string]any{"id": "otherbot", "bot": true},
	}
	fromSelf := map[string]any{
		"id": "m3", "channel_id": "c1", "content": "my own ack",
		"author": map[string]any{"id": "self1", "bot": false},
	}

	gotIdentify := make(chan json.RawMessage, 1)
	gotHeartbeat := make(chan struct{}, 1)
	srv := mockGateway(t, []payload{
		dispatch(t, "READY", map[string]any{}),
		dispatch(t, "MESSAGE_CREATE", me),
		dispatch(t, "MESSAGE_CREATE", fromBot),
		dispatch(t, "MESSAGE_CREATE", fromSelf),
	}, gotIdentify, gotHeartbeat)
	defer srv.Close()

	msgs := make(chan Message, 8)
	g := &Gateway{
		URL:       "ws" + strings.TrimPrefix(srv.URL, "http"),
		Token:     "tok",
		SelfID:    "self1",
		OnMessage: func(m Message) { msgs <- m },
		Logf:      t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = g.Run(ctx) }()

	select {
	case raw := <-gotIdentify:
		var id struct {
			Token   string `json:"token"`
			Intents int    `json:"intents"`
		}
		if err := json.Unmarshal(raw, &id); err != nil || id.Token != "tok" || id.Intents != intents {
			t.Fatalf("identify = %s (err %v)", raw, err)
		}
	case <-ctx.Done():
		t.Fatal("no IDENTIFY before timeout")
	}

	// Only the human message arrives; READY, bots and self are dropped.
	select {
	case m := <-msgs:
		if m.Content != "hello agent" || m.ChannelID != "c1" {
			t.Fatalf("delivered %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("no message delivered before timeout")
	}
	select {
	case m := <-msgs:
		t.Fatalf("bot/self message leaked through: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case <-gotHeartbeat:
	case <-ctx.Done():
		t.Fatal("no heartbeat before timeout")
	}
}

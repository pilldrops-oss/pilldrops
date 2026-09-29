package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultGatewayURL is Discord's Gateway endpoint. Overridden in tests.
const DefaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

// MESSAGE_CONTENT is a privileged intent; without it enabled in the developer
// portal every message arrives with empty text.
const intents = 1<<9 | 1<<12 | 1<<15 // GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT

// Message is the slice of a Discord MESSAGE_CREATE the bridge acts on.
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Author    struct {
		ID  string `json:"id"`
		Bot bool   `json:"bot"`
	} `json:"author"`
}

// payload is the Gateway frame: opcode plus event data, with name and seq on dispatches.
type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// RESUME is deliberately not implemented: any close reconnects and re-identifies,
// accepting a small window of missed messages.
const (
	opDispatch  = 0
	opHeartbeat = 1
	opIdentify  = 2
	opHello     = 10
)

// Gateway consumes the Discord WebSocket and delivers MESSAGE_CREATE events to OnMessage.
type Gateway struct {
	URL       string
	Token     string
	SelfID    string // bot's own user id; its echoes are dropped here
	OnMessage func(Message)
	Logf      func(string, ...any)
}

// Run consumes until ctx is cancelled, reconnecting with backoff on any failure.
func (g *Gateway) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := g.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		g.Logf("gateway session ended: %v — reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (g *Gateway) session(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, g.URL, nil) //nolint:bodyclose // the handshake response body is owned by the websocket conn, closed below
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// The gateway speaks first; HELLO carries the heartbeat interval.
	var hello payload
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if hello.Op != opHello {
		return fmt.Errorf("expected HELLO (op 10), got op %d", hello.Op)
	}
	var hd struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &hd); err != nil || hd.HeartbeatInterval <= 0 {
		return fmt.Errorf("bad HELLO payload: %w", err)
	}

	identify, _ := json.Marshal(map[string]any{
		"token":   g.Token,
		"intents": intents,
		"properties": map[string]string{
			"os": "linux", "browser": "pilldrops-bridge", "device": "pilldrops-bridge",
		},
	})
	if err := conn.WriteJSON(payload{Op: opIdentify, D: identify}); err != nil {
		return fmt.Errorf("identify: %w", err)
	}

	// gorilla allows one concurrent writer, so heartbeats own the socket writes.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	hbErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(time.Duration(hd.HeartbeatInterval) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := conn.WriteJSON(payload{Op: opHeartbeat}); err != nil {
					hbErr <- err
					return
				}
			}
		}
	}()

	// A websocket read takes no context, so unblock ReadJSON via the deadline.
	go func() {
		<-hbCtx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	for {
		select {
		case err := <-hbErr:
			return fmt.Errorf("heartbeat: %w", err)
		default:
		}
		var p payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if p.Op != opDispatch || p.T != "MESSAGE_CREATE" {
			continue // heartbeat ACKs, READY, presence noise
		}
		var m Message
		if err := json.Unmarshal(p.D, &m); err != nil {
			g.Logf("bad MESSAGE_CREATE: %v", err)
			continue
		}
		// Ignore bots, including this one, or the bridge loops on its own acks.
		if m.Author.Bot || m.Author.ID == g.SelfID {
			continue
		}
		if g.OnMessage != nil {
			g.OnMessage(m)
		}
	}
}

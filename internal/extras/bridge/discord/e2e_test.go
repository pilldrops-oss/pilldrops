package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/bridge/telegram"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// mockDiscordREST records what the bridge posts back to Discord.
type mockDiscordREST struct {
	mu   sync.Mutex
	msgs []string
	srv  *httptest.Server
}

func newMockDiscordREST() *mockDiscordREST {
	m := &mockDiscordREST{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &p)
		m.mu.Lock()
		m.msgs = append(m.msgs, p.Content)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg%d"}`, len(m.msgs))
	}))
	return m
}

// waitFor polls until a recorded message contains want.
func (m *mockDiscordREST) waitFor(t *testing.T, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		for _, s := range m.msgs {
			if strings.Contains(s, want) {
				m.mu.Unlock()
				return s
			}
		}
		m.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.Fatalf("no Discord reply containing %q; got %v", want, m.msgs)
	return ""
}

// TestDiscordEndToEndAgainstLiveNode runs the full chain against a live node
// with only Discord's servers mocked:
//
//	PD_E2E_BASE=http://localhost:8399 PD_E2E_KEY=... go test ./internal/extras/bridge/discord -run EndToEnd
func TestDiscordEndToEndAgainstLiveNode(t *testing.T) {
	base, key := os.Getenv("PD_E2E_BASE"), os.Getenv("PD_E2E_KEY")
	if base == "" || key == "" {
		t.Skip("set PD_E2E_BASE / PD_E2E_KEY to run against a live node (tests/e2e/full-feature-e2e.sh does)")
	}

	req, _ := http.NewRequest("POST", base+"/v1/sandboxes", bytes.NewReader([]byte(`{"template":"base"}`)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	var sb struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	if sb.ID == "" {
		t.Fatalf("no sandbox id (status %d)", resp.StatusCode)
	}
	t.Cleanup(func() {
		dreq, _ := http.NewRequest("DELETE", base+"/v1/sandboxes/"+sb.ID, nil)
		dreq.Header.Set("Authorization", "Bearer "+key)
		if r, err := http.DefaultClient.Do(dreq); err == nil {
			r.Body.Close()
		}
	})

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	code, err := st.CreateLinkCode(t.Context(), sb.ID, "default", false, false)
	if err != nil {
		t.Fatal(err)
	}

	rest := newMockDiscordREST()
	t.Cleanup(rest.srv.Close)
	client := NewClient("test-token")
	client.APIBase = rest.srv.URL
	boxes := telegram.NewAPIBoxes(base, key)
	b := New(client, st, &telegram.DaemonRunner{Boxes: boxes})
	b.Logf = t.Logf

	gotIdentify := make(chan json.RawMessage, 1)
	gotHeartbeat := make(chan struct{}, 1)
	user := func(text string) payload {
		return dispatch(t, "MESSAGE_CREATE", map[string]any{
			"id": "m", "channel_id": "dm42", "content": text,
			"author": map[string]any{"id": "human1", "bot": false},
		})
	}
	gw := mockGateway(t, []payload{
		user("!link " + code),
		user("echo hello from my phone"),
	}, gotIdentify, gotHeartbeat)
	t.Cleanup(gw.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	g := &Gateway{
		URL:    "ws" + strings.TrimPrefix(gw.URL, "http"),
		Token:  "test-token",
		SelfID: "self1",
		// Synchronous so the second message cannot race the first one's link redemption.
		OnMessage: func(m Message) { b.Handle(ctx, m) },
		Logf:      t.Logf,
	}
	go func() { _ = g.Run(ctx) }()

	rest.waitFor(t, "Connected to "+sb.ID, 30*time.Second)
	if link, ok, _ := st.ChatLinkFor(t.Context(), Channel, "dm42"); !ok || link.SandboxID != sb.ID {
		t.Fatalf("link not stored: %+v ok=%v", link, ok)
	}

	// The base template ships no agent CLI, so the runner's verdict is "not chat-capable".
	rest.waitFor(t, "Waking your workstation", 30*time.Second)
	rest.waitFor(t, "chat-capable", 60*time.Second)
}

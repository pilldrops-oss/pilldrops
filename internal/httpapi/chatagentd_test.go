//go:build !core

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
)

// stubAgentd serves just enough of agentd's surface for the chat wait loop.
// Mutex-guarded: the test goroutine writes while the handler reads.
type stubAgentd struct {
	mu        sync.Mutex
	turn      agentd.Turn
	pending   []map[string]any
	approvals int
}

func (st *stubAgentd) setTurn(t agentd.Turn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.turn = t
}

func (st *stubAgentd) start(t *testing.T) *agentd.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/turns/", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		turn := st.turn
		st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(turn)
	})
	mux.HandleFunc("/approvals", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.approvals++
		pending := st.pending
		st.mu.Unlock()
		// "approvals" is the key the real agentd handler uses.
		_ = json.NewEncoder(w).Encode(map[string]any{"approvals": pending})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &agentd.Client{Addr: strings.TrimPrefix(srv.URL, "http://")}
}

func fastChatTiming(t *testing.T) {
	t.Helper()
	poll, grace := chatPollInterval, awaitingGrace
	chatPollInterval, awaitingGrace = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { chatPollInterval, awaitingGrace = poll, grace })
}

func TestParkedApprovalAnswersInsteadOfHanging(t *testing.T) {
	fastChatTiming(t)
	st := &stubAgentd{
		turn: agentd.Turn{ID: "t1", State: agentd.TurnAwaiting},
		pending: []map[string]any{
			{"turn_id": "t1", "signature": "git push", "tool": "Bash"},
		},
	}
	c := st.start(t)

	done := make(chan string, 1)
	go func() {
		reply, err := (&Server{}).waitForChatTurn(context.Background(), c, "t1")
		if err != nil {
			done <- "ERROR: " + err.Error()
			return
		}
		done <- reply
	}()

	select {
	case reply := <-done:
		if strings.HasPrefix(reply, "ERROR:") {
			t.Fatalf("a parked approval was reported as a failure: %s", reply)
		}
		if !strings.Contains(reply, "git push") {
			t.Errorf("reply does not name what is parked, so the user cannot decide: %q", reply)
		}
		if !strings.Contains(reply, "queued") {
			t.Errorf("reply does not say the task survives, so it reads as lost work: %q", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForChatTurn hung on a parked approval")
	}
}

func TestParkedReplyIgnoresAnotherTurnsApproval(t *testing.T) {
	fastChatTiming(t)
	st := &stubAgentd{
		turn: agentd.Turn{ID: "t1", State: agentd.TurnAwaiting},
		pending: []map[string]any{
			{"turn_id": "t2", "signature": "rm -rf build"},
		},
	}
	c := st.start(t)
	reply := (&Server{}).parkedReply(context.Background(), c, &st.turn)
	if strings.Contains(reply, "rm -rf build") {
		t.Error("named another turn's approval, inviting the user to approve the wrong command")
	}
	if reply == "" {
		t.Error("said nothing at all")
	}
}

// An approval granted inside the grace window comes back as a normal reply.
func TestAwaitingThenDoneReturnsTheAnswer(t *testing.T) {
	fastChatTiming(t)
	st := &stubAgentd{turn: agentd.Turn{ID: "t1", State: agentd.TurnAwaiting}}
	c := st.start(t)
	go func() {
		time.Sleep(8 * time.Millisecond)
		st.setTurn(agentd.Turn{ID: "t1", State: agentd.TurnDone, Response: "pushed"})
	}()
	reply, err := (&Server{}).waitForChatTurn(context.Background(), c, "t1")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if reply != "pushed" {
		t.Errorf("got %q, want the agent's answer", reply)
	}
}

func TestInterruptedTurnReturnsPartialWork(t *testing.T) {
	fastChatTiming(t)
	st := &stubAgentd{turn: agentd.Turn{
		ID: "t1", State: agentd.TurnInterrupted, Response: "got halfway",
	}}
	reply, err := (&Server{}).waitForChatTurn(context.Background(), st.start(t), "t1")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if reply != "got halfway" {
		t.Errorf("got %q, want the partial output", reply)
	}
}

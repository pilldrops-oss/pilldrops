package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// fakeAPI serves the slice of the agent API that RunTurn uses.
type fakeAPI struct {
	mu        sync.Mutex
	available []string
	state     string
	response  string
	turnErr   string
	pending   []map[string]any
	submitted map[string]any
	source    string
	submits   int
	notFound  bool
}

func (f *fakeAPI) start(t *testing.T) *APIBoxes {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandboxes/sb1/agent/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": map[string]any{"autonomy": map[string]any{"available": f.available}},
		})
	})
	mux.HandleFunc("/v1/sandboxes/sb1/agent/turns", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.submits++
		f.source = r.Header.Get("X-Agent-Source")
		if f.notFound {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "sandbox not found"})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&f.submitted)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"turn_id": "tn1", "state": "queued"})
	})
	mux.HandleFunc("/v1/sandboxes/sb1/agent/turns/tn1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": f.state, "response": f.response, "error": f.turnErr,
		})
	})
	mux.HandleFunc("/v1/sandboxes/sb1/agent/approvals", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"approvals": f.pending})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := NewAPIBoxes(srv.URL, "k")
	a.Source = "telegram"
	return a
}

func fastTurnTiming(t *testing.T) {
	t.Helper()
	poll, grace := turnPoll, turnAwaitingGrace
	turnPoll, turnAwaitingGrace = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { turnPoll, turnAwaitingGrace = poll, grace })
}

// Submitting a non-autonomous task as autonomous would widen every chat link to full write access.
func TestRunTurnUsesGuidedForANonAutonomousTask(t *testing.T) {
	fastTurnTiming(t)
	f := &fakeAPI{available: []string{"supervised", "guided", "autonomous"}, state: "done", response: "ok"}
	a := f.start(t)

	reply, err := a.RunTurn(context.Background(), "sb1", "what changed?", RunOpts{Autonomous: false})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if reply != "ok" {
		t.Errorf("reply = %q", reply)
	}
	if got := f.submitted["mode"]; got != "guided" {
		t.Errorf("mode = %v, want guided — a read-only chat link was widened", got)
	}
	// Without the header the turn records as "api".
	if f.source != "telegram" {
		t.Errorf("X-Agent-Source = %q, want telegram", f.source)
	}
}

func TestRunTurnFallsBackWhenGuidedIsUnavailable(t *testing.T) {
	fastTurnTiming(t)
	f := &fakeAPI{available: []string{"autonomous"}, state: "done", response: "ok"}
	a := f.start(t)

	_, err := a.RunTurn(context.Background(), "sb1", "what changed?", RunOpts{})
	if !errors.Is(err, ErrTurnsUnavailable) {
		t.Fatalf("err = %v, want ErrTurnsUnavailable", err)
	}
	if f.submits != 0 {
		t.Error("submitted a turn anyway, in some other mode")
	}
}

func TestRunTurnAutonomousSkipsTheGateCheck(t *testing.T) {
	fastTurnTiming(t)
	f := &fakeAPI{available: nil, state: "done", response: "done it"}
	a := f.start(t)

	reply, err := a.RunTurn(context.Background(), "sb1", "fix it", RunOpts{Autonomous: true})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if reply != "done it" || f.submitted["mode"] != "autonomous" {
		t.Errorf("reply=%q mode=%v", reply, f.submitted["mode"])
	}
}

func TestRunTurnFollowUpSelectsTheThread(t *testing.T) {
	fastTurnTiming(t)
	for _, tc := range []struct {
		followUp bool
		want     string
	}{
		{true, "latest"},
		{false, "new"},
	} {
		f := &fakeAPI{available: []string{"guided"}, state: "done", response: "ok"}
		a := f.start(t)
		if _, err := a.RunTurn(context.Background(), "sb1", "hi", RunOpts{FollowUp: tc.followUp}); err != nil {
			t.Fatalf("RunTurn: %v", err)
		}
		if got := f.submitted["thread"]; got != tc.want {
			t.Errorf("followUp=%v gave thread=%v, want %s", tc.followUp, got, tc.want)
		}
	}
}

func TestRunTurnParkedApprovalIsExplained(t *testing.T) {
	fastTurnTiming(t)
	f := &fakeAPI{
		available: []string{"guided"},
		state:     "awaiting_approval",
		pending:   []map[string]any{{"turn_id": "tn1", "signature": "git push"}},
	}
	a := f.start(t)

	done := make(chan string, 1)
	go func() {
		reply, err := a.RunTurn(context.Background(), "sb1", "push it", RunOpts{})
		if err != nil {
			done <- "ERROR: " + err.Error()
			return
		}
		done <- reply
	}()
	select {
	case reply := <-done:
		if !strings.Contains(reply, "git push") {
			t.Errorf("reply does not name what is parked: %q", reply)
		}
		if !strings.Contains(reply, "queued") {
			t.Errorf("reply does not say the task survives: %q", reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn hung on a parked approval")
	}
}

func TestRunTurnUnsupportedSandboxFallsBack(t *testing.T) {
	fastTurnTiming(t)
	f := &fakeAPI{available: []string{"guided"}, notFound: true}
	a := f.start(t)
	_, err := a.RunTurn(context.Background(), "sb1", "hi", RunOpts{})
	if !errors.Is(err, ErrTurnsUnavailable) {
		t.Fatalf("err = %v, want ErrTurnsUnavailable so the exec path runs", err)
	}
}

// Retrying a spend-cap refusal on the exec path would bill past the cap.
func TestRunTurnRefusalIsNotRetried(t *testing.T) {
	fastTurnTiming(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandboxes/sb1/agent/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": map[string]any{"autonomy": map[string]any{"available": []string{"guided"}}},
		})
	})
	mux.HandleFunc("/v1/sandboxes/sb1/agent/turns", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "this agent has spent its $5.00 budget for the day",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := NewAPIBoxes(srv.URL, "k").RunTurn(context.Background(), "sb1", "hi", RunOpts{})
	if err == nil {
		t.Fatal("a spend-cap refusal was swallowed")
	}
	if errors.Is(err, ErrTurnsUnavailable) {
		t.Fatal("a spend-cap refusal would be retried on the exec path, spending past the cap")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("the refusal lost its explanation: %v", err)
	}
}

// execOnlyBoxes is a Sandboxes that records whether the exec path was taken.
type execOnlyBoxes struct{ execs int }

func (b *execOnlyBoxes) Get(string) (SandboxState, bool) {
	return SandboxState{Template: "claude"}, true
}
func (b *execOnlyBoxes) Resume(context.Context, string) error { return nil }
func (b *execOnlyBoxes) Exec(context.Context, string, rt.ExecSpec) (rt.ExecResult, error) {
	b.execs++
	return rt.ExecResult{Stdout: "from exec"}, nil
}

// stubTurns lets the runner test drive the decision without HTTP.
type stubTurns struct {
	reply string
	err   error
	calls int
}

func (s *stubTurns) RunTurn(context.Context, string, string, RunOpts) (string, error) {
	s.calls++
	return s.reply, s.err
}

func TestRunnerPrefersTurnsAndFallsBackOnlyWhenUnavailable(t *testing.T) {
	boxes := &execOnlyBoxes{}
	turns := &stubTurns{reply: "from agentd"}
	r := &DaemonRunner{Boxes: boxes, Turns: turns, Gate: NoGate}

	got, err := r.Run(context.Background(), "sb1", "hi", RunOpts{Autonomous: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "from agentd" {
		t.Errorf("reply = %q, want the agentd path", got)
	}
	if boxes.execs != 0 {
		t.Error("ran the exec path as well, so the turn happened outside the queue")
	}

	// Unavailable falls back to exec.
	turns.err = ErrTurnsUnavailable
	got, err = r.Run(context.Background(), "sb1", "hi", RunOpts{Autonomous: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "from exec" || boxes.execs != 1 {
		t.Errorf("reply = %q execs = %d, want the exec fallback", got, boxes.execs)
	}

	// A real error is surfaced, not retried on exec.
	turns.err = errors.New("this agent has spent its budget")
	if _, err = r.Run(context.Background(), "sb1", "hi", RunOpts{Autonomous: true}); err == nil {
		t.Fatal("a refusal was swallowed")
	}
	if boxes.execs != 1 {
		t.Error("a refusal was retried on the exec path, bypassing the limit that refused it")
	}
}

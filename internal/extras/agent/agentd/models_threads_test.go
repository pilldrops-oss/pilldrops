package agentd

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestModelsReportsUnsupportedRatherThanAnEmptyList(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })
	srv := &Server{
		Sup: sup, Journal: sup.Journal, Queue: sup.Queue,
		CurrentModel: func() string { return "claude-opus-4-8" },
		// Models deliberately nil.
	}
	ts := newServerFor(t, srv)

	resp, body := req(t, ts, "GET", "/models", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models = %d, want 200", resp.StatusCode)
	}
	if body["supported"] != false {
		t.Errorf("supported = %v, want false", body["supported"])
	}
	if models, _ := body["models"].([]any); len(models) != 0 {
		t.Errorf("models = %v, want empty", models)
	}
	if body["current"] != "claude-opus-4-8" {
		t.Errorf("current = %v", body["current"])
	}
	if reason, _ := body["reason"].(string); reason == "" {
		t.Error("no reason given, so a UI cannot explain why the picker is empty")
	}
}

func TestModelsListsWhenTheAgentCan(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })
	srv := &Server{
		Sup: sup, Journal: sup.Journal, Queue: sup.Queue,
		Models: func(context.Context) ([]string, error) {
			return []string{"opencode/big-pickle", "opencode/deepseek-v4-flash-free"}, nil
		},
	}
	ts := newServerFor(t, srv)

	_, body := req(t, ts, "GET", "/models", "", "")
	if body["supported"] != true {
		t.Fatalf("supported = %v, want true", body["supported"])
	}
	models, _ := body["models"].([]any)
	if len(models) != 2 || models[0] != "opencode/big-pickle" {
		t.Errorf("models = %v", models)
	}
}

func TestModelsListingFailureIsReportedNotFatal(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `true` })
	srv := &Server{
		Sup: sup, Journal: sup.Journal, Queue: sup.Queue,
		Models: func(context.Context) ([]string, error) { return nil, ErrNotFound },
	}
	ts := newServerFor(t, srv)

	resp, body := req(t, ts, "GET", "/models", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a failed listing returned %d; it should still answer", resp.StatusCode)
	}
	if body["supported"] != false {
		t.Errorf("supported = %v, want false after a failure", body["supported"])
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "could not list") {
		t.Errorf("reason = %q", reason)
	}
}

func TestThreadsAreGroupedByTheAgentsOwnSessionID(t *testing.T) {
	q := openTestQueue(t, t.TempDir())

	a := enqueue(t, q, "first in thread one")
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetSessionID(a.ID, "sess_one"); err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(a.ID, TurnDone, ptr(0), "", 1); err != nil {
		t.Fatal(err)
	}

	b := enqueue(t, q, "second in thread one")
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetSessionID(b.ID, "sess_one"); err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(b.ID, TurnDone, ptr(0), "", 2); err != nil {
		t.Fatal(err)
	}

	c := enqueue(t, q, "only turn in thread two")
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetSessionID(c.ID, "sess_two"); err != nil {
		t.Fatal(err)
	}

	threads := q.Threads()
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2: %+v", len(threads), threads)
	}
	byID := map[string]Thread{}
	for _, th := range threads {
		byID[th.ID] = th
	}
	if byID["sess_one"].Turns != 2 {
		t.Errorf("thread one has %d turns, want 2", byID["sess_one"].Turns)
	}
	// The newest request labels the thread.
	if byID["sess_one"].Title != "second in thread one" {
		t.Errorf("thread title = %q", byID["sess_one"].Title)
	}
	if byID["sess_two"].Turns != 1 {
		t.Errorf("thread two has %d turns, want 1", byID["sess_two"].Turns)
	}

	turns := q.TurnsForThread("sess_one")
	if len(turns) != 2 {
		t.Errorf("TurnsForThread = %d turns, want 2", len(turns))
	}
}

func TestTurnsWithoutASessionIDAreStillAddressable(t *testing.T) {
	sup := newTestSupervisor(t, func(*Turn) string { return `printf ok` })
	stop := runLoop(t, sup)
	defer stop()

	turn, err := sup.Submit("no session id", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to finish", func() bool {
		got, err := sup.Queue.Get(turn.ID)
		return err == nil && got.State == TurnDone
	})

	threads := sup.Queue.Threads()
	if len(threads) != 1 || threads[0].ID != "" {
		t.Fatalf("threads = %+v, want one with an empty id", threads)
	}

	srv := &Server{Sup: sup, Journal: sup.Journal, Queue: sup.Queue}
	ts := newServerFor(t, srv)

	// "unknown" addresses the empty-id thread, since "" cannot be a path segment.
	resp, body := req(t, ts, "GET", "/threads/unknown", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /threads/unknown = %d, want 200", resp.StatusCode)
	}
	if turns, _ := body["turns"].([]any); len(turns) != 1 {
		t.Errorf("turns = %v, want 1", body["turns"])
	}

	if resp, _ := req(t, ts, "GET", "/threads/sess_nope", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown thread = %d, want 404", resp.StatusCode)
	}
}

func TestSessionIDSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, dir)
	turn := enqueue(t, q, "task")
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetSessionID(turn.ID, "sess_keep"); err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(turn.ID, TurnDone, ptr(0), "", 1); err != nil {
		t.Fatal(err)
	}

	reopened := openTestQueue(t, dir)
	got, err := reopened.Get(turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess_keep" {
		t.Errorf("session id = %q after restart, want sess_keep", got.SessionID)
	}
}

func TestModelListingSkipsProseAndDeduplicates(t *testing.T) {
	tp := &Templates{
		RunCmd: `true`,
		ModelsCmd: `printf '%s\n' 'Available models:' 'opencode/one' '' 'opencode/one' ` +
			`'opencode/two' 'some prose line with spaces'`,
	}
	models, err := tp.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	want := []string{"opencode/one", "opencode/two"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models = %v, want %v", models, want)
			break
		}
	}
}

func TestModelsWithNoCommandIsAnError(t *testing.T) {
	tp := &Templates{RunCmd: `true`}
	if _, err := tp.Models(context.Background()); err == nil {
		t.Error("listing with no ModelsCmd succeeded")
	}
}

func TestModelListingIsTimeBounded(t *testing.T) {
	tp := &Templates{RunCmd: `true`, ModelsCmd: `sleep 30`}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := tp.Models(ctx); err == nil {
		t.Error("a hung listing command returned success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("listing took %v; the caller's deadline was not honoured", elapsed)
	}
}

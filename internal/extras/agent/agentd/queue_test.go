package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestQueue(t *testing.T, dir string) *Queue {
	t.Helper()
	q, err := OpenQueue(dir)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	return q
}

func enqueue(t *testing.T, q *Queue, req string) *Turn {
	t.Helper()
	turn, err := q.Enqueue(req, ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatalf("enqueue %q: %v", req, err)
	}
	return turn
}

func TestClaimRefusesWhileATurnIsRunning(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	enqueue(t, q, "first")
	enqueue(t, q, "second")

	first, err := q.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("nothing claimed from a queue with two turns")
	}

	again, err := q.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("claimed %s while %s was still running", again.ID, first.ID)
	}

	if err := q.Finish(first.ID, TurnDone, ptr(0), "", 42); err != nil {
		t.Fatal(err)
	}
	second, err := q.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Request != "second" {
		t.Fatalf("after finishing the first, claimed %+v, want the second turn", second)
	}
}

// A parked turn's agent process is still alive and holding the transcript.
func TestAwaitingApprovalStillBlocksTheQueue(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	first := enqueue(t, q, "needs approval")
	enqueue(t, q, "behind it")

	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetState(first.ID, TurnAwaiting); err != nil {
		t.Fatal(err)
	}
	got, err := q.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("claimed %s while a turn was parked for approval", got.ID)
	}
}

func TestServiceOrderIsFIFOWithPriorityFirst(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	a := enqueue(t, q, "a")
	b := enqueue(t, q, "b")
	urgent, err := q.Enqueue("urgent", ModeAutonomous, "latest", "api", true)
	if err != nil {
		t.Fatal(err)
	}
	c := enqueue(t, q, "c")

	want := []string{urgent.ID, a.ID, b.ID, c.ID}
	pending := q.Pending()
	if len(pending) != len(want) {
		t.Fatalf("pending = %d turns, want %d", len(pending), len(want))
	}
	for i, id := range want {
		if pending[i].ID != id {
			t.Fatalf("position %d = %q, want %q", i, pending[i].Request, id)
		}
	}
}

func TestQueuedTurnsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	q1 := openTestQueue(t, dir)
	a := enqueue(t, q1, "first")
	b := enqueue(t, q1, "second")

	q2 := openTestQueue(t, dir)
	pending := q2.Pending()
	if len(pending) != 2 {
		t.Fatalf("after restart %d turns pending, want 2", len(pending))
	}
	if pending[0].ID != a.ID || pending[1].ID != b.ID {
		t.Errorf("restart lost the queue order: got %s,%s want %s,%s",
			pending[0].ID, pending[1].ID, a.ID, b.ID)
	}
	if pending[0].Request != "first" {
		t.Errorf("turn content did not survive: %q", pending[0].Request)
	}
}

func TestRunningTurnFailsRatherThanRepeatingOnRestart(t *testing.T) {
	dir := t.TempDir()
	q1 := openTestQueue(t, dir)
	turn := enqueue(t, q1, "push the branch")
	if _, err := q1.Claim(); err != nil {
		t.Fatal(err)
	}
	// No Finish: the supervisor died mid-turn.

	q2 := openTestQueue(t, dir)
	got, err := q2.Get(turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TurnFailed {
		t.Errorf("interrupted-by-restart turn is %s, want %s — re-running it could repeat its effects",
			got.State, TurnFailed)
	}
	if got.Error == "" {
		t.Error("no explanation recorded; the user cannot tell what happened")
	}
	if got.EndedAt == nil {
		t.Error("no end time recorded on a turn that will never continue")
	}
	for _, p := range q2.Pending() {
		if p.ID == turn.ID {
			t.Error("the failed turn is back in the pending queue")
		}
	}
}

func TestQueueDepthIsBounded(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	q.maxPending = 3
	for i := 0; i < 3; i++ {
		enqueue(t, q, "task")
	}
	if _, err := q.Enqueue("one too many", ModeAutonomous, "latest", "api", false); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("enqueue past the cap = %v, want ErrQueueFull", err)
	}
	// The cap is on waiting work, not total work.
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue("now there is room", ModeAutonomous, "latest", "api", false); err != nil {
		t.Errorf("enqueue after a claim freed a slot: %v", err)
	}
}

func TestRemoveDequeuesOnlyUnstartedTurns(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	queued := enqueue(t, q, "not started")
	running := enqueue(t, q, "will run")

	// Reorder so Claim picks running rather than the FIFO head.
	if err := q.Reorder([]string{running.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}

	if err := q.Remove(queued.ID); err != nil {
		t.Errorf("removing an unstarted turn: %v", err)
	}
	if _, err := q.Get(queued.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed turn is still present: %v", err)
	}
	if err := q.Remove(running.ID); err == nil {
		t.Error("removing a running turn was allowed")
	}
}

func TestReorderPutsNamedTurnsFirstAndKeepsTheRest(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	a := enqueue(t, q, "a")
	b := enqueue(t, q, "b")
	c := enqueue(t, q, "c")

	if err := q.Reorder([]string{c.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	pending := q.Pending()
	want := []string{c.ID, a.ID, b.ID}
	for i, id := range want {
		if pending[i].ID != id {
			t.Fatalf("after reorder position %d = %q, want %q", i, pending[i].Request, id)
		}
	}
	if got := openTestQueue(t, q.dir).Pending(); got[0].ID != c.ID {
		t.Errorf("reorder did not survive restart: head is %s, want %s", got[0].ID, c.ID)
	}
}

func TestReorderRejectsUnknownAndDuplicateIDs(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	a := enqueue(t, q, "a")

	if err := q.Reorder([]string{"turn_nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("reorder with an unknown id = %v, want ErrNotFound", err)
	}
	if err := q.Reorder([]string{a.ID, a.ID}); err == nil {
		t.Error("reorder accepted the same id twice")
	}
}

func TestTurnRecordsAreWrittenAtomically(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, dir)
	turn := enqueue(t, q, "a")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, turn.ID+".json")); err != nil {
		t.Errorf("turn record missing: %v", err)
	}
}

func TestCorruptTurnRecordIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t, dir)
	good := enqueue(t, q, "good")

	if err := os.WriteFile(filepath.Join(dir, "turn_corrupt.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenQueue(dir)
	if err != nil {
		t.Fatalf("a corrupt record made the queue unopenable: %v", err)
	}
	pending := reopened.Pending()
	if len(pending) != 1 || pending[0].ID != good.ID {
		t.Errorf("expected only the good turn, got %+v", pending)
	}
}

func TestListReturnsCopiesNotLiveState(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	turn := enqueue(t, q, "original")

	list := q.List()
	list[0].Request = "tampered"
	list[0].State = TurnDone

	got, err := q.Get(turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Request != "original" || got.State != TurnQueued {
		t.Errorf("queue state was mutated through a returned pointer: %+v", got)
	}
}

func TestTurnIDsAreUniqueWithinAMillisecond(t *testing.T) {
	now := time.Now()
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewTurnID(now)
		if seen[id] {
			t.Fatalf("duplicate turn id %s", id)
		}
		seen[id] = true
	}
}

func TestConcurrentEnqueueAndClaimStaySingleWriter(t *testing.T) {
	q := openTestQueue(t, t.TempDir())
	q.maxPending = 200

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				if _, err := q.Enqueue("task", ModeAutonomous, "latest", "api", false); err != nil &&
					!errors.Is(err, ErrQueueFull) {
					t.Errorf("concurrent enqueue: %v", err)
					return
				}
			}
		}()
	}
	var claimed int
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				got, err := q.Claim()
				if err != nil {
					t.Errorf("concurrent claim: %v", err)
					return
				}
				if got != nil {
					mu.Lock()
					claimed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// Nothing finished a turn, so at most one was ever claimed.
	if claimed > 1 {
		t.Errorf("%d turns were claimed concurrently; single-writer was violated", claimed)
	}
	running := 0
	for _, turn := range q.List() {
		if turn.State == TurnRunning || turn.State == TurnAwaiting {
			running++
		}
	}
	if running > 1 {
		t.Errorf("%d turns are in a running state at once", running)
	}
}

func ptr[T any](v T) *T { return &v }

// Tested through Submit, not Queue.SetSeqStart: Enqueue returns a clone, so the
// bug is a caller that sets the field without persisting it.
func TestSubmitPersistsTheJournalCursor(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return "echo ok" })

	turn, err := s.Submit("do the thing", ModeAutonomous, "", "api", false)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if turn.SeqStart == 0 {
		t.Fatal("the submit response carried no cursor")
	}

	// Read back from the queue, not the returned clone.
	stored, err := s.Queue.Get(turn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.SeqStart != turn.SeqStart {
		t.Errorf("stored seq_start = %d but the response said %d — a client that fetches this "+
			"turn later cannot replay it", stored.SeqStart, turn.SeqStart)
	}
}

func TestSeqStartIsForwardOnly(t *testing.T) {
	q, err := OpenQueue(t.TempDir())
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	turn, _ := q.Enqueue("x", ModeAutonomous, "", "api", false)
	if err := q.SetSeqStart(turn.ID, 100); err != nil {
		t.Fatalf("SetSeqStart: %v", err)
	}
	if err := q.SetSeqStart(turn.ID, 7); err != nil {
		t.Fatalf("SetSeqStart backwards returned an error rather than being ignored: %v", err)
	}
	got, err := q.Get(turn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SeqStart != 100 {
		t.Errorf("seq_start moved backwards to %d", got.SeqStart)
	}
}

package agentd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newTestSupervisor wires a real queue, journal and runner over a per-test shell script.
func newTestSupervisor(t *testing.T, script func(*Turn) string) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	j := openTestJournal(t, dir+"/journal")
	q, err := OpenQueue(dir + "/queue")
	if err != nil {
		t.Fatal(err)
	}
	return &Supervisor{
		Queue:   q,
		Journal: j,
		Runner: &Runner{
			Journal: j,
			Build:   func(turn *Turn) (string, []string, error) { return script(turn), nil, nil },
			Grace:   200 * time.Millisecond,
		},
		Mode:         ModeRich,
		AgentVersion: "test-1.0",
		PollInterval: 20 * time.Millisecond,
	}
}

// runLoop starts the drain loop and returns a stop function.
func runLoop(t *testing.T, s *Supervisor) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(ctx); err != nil {
			t.Errorf("supervisor loop: %v", err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("supervisor loop did not stop")
		}
	}
}

// waitFor polls until cond holds, failing the test on timeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupervisorRunsAQueuedTurnToCompletion(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `printf 'done'` })
	stop := runLoop(t, s)
	defer stop()

	turn, err := s.Submit("do the thing", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if turn.SeqStart == 0 {
		t.Error("Submit did not journal the turn, so a client cannot stream from it")
	}

	waitFor(t, "the turn to finish", func() bool {
		got, err := s.Queue.Get(turn.ID)
		return err == nil && got.State == TurnDone
	})
	got, _ := s.Queue.Get(turn.ID)
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}
	if got.SeqEnd == 0 {
		t.Error("no end sequence recorded; a client cannot tell where the turn stopped")
	}
}

func TestSupervisorDrainsTheWholeQueueOneTurnAtATime(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `printf 'x'; sleep 0.05` })
	stop := runLoop(t, s)
	defer stop()

	var ids []string
	for i := 0; i < 3; i++ {
		turn, err := s.Submit("task", ModeAutonomous, "latest", "api", false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, turn.ID)
	}

	waitFor(t, "all three turns to finish", func() bool {
		for _, id := range ids {
			got, err := s.Queue.Get(id)
			if err != nil || got.State != TurnDone {
				return false
			}
		}
		return true
	})

	// Run windows must be disjoint.
	for i, id := range ids {
		a, _ := s.Queue.Get(id)
		for j, other := range ids {
			if i >= j {
				continue
			}
			b, _ := s.Queue.Get(other)
			if a.StartedAt.Before(*b.EndedAt) && b.StartedAt.Before(*a.EndedAt) {
				t.Errorf("turns %d and %d ran concurrently", i, j)
			}
		}
	}
}

func TestFailingTurnDoesNotStopTheLoopOrDegradeTheSession(t *testing.T) {
	var n int
	s := newTestSupervisor(t, func(*Turn) string {
		n++
		if n == 1 {
			return `printf 'no api key' >&2; exit 1`
		}
		return `printf 'fine'`
	})
	stop := runLoop(t, s)
	defer stop()

	bad, err := s.Submit("first", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failing turn to be recorded", func() bool {
		got, err := s.Queue.Get(bad.ID)
		return err == nil && got.State == TurnFailed
	})

	if st := s.Status(); st.State == StateDegraded {
		t.Errorf("a non-zero agent exit marked the session degraded (%q) — that cries wolf", st.DegradedReason)
	}

	good, err := s.Submit("second", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the loop to keep serving after a failure", func() bool {
		got, err := s.Queue.Get(good.ID)
		return err == nil && got.State == TurnDone
	})
}

func TestUnstartableTurnDegradesTheSessionWithAReason(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return "" })
	s.Runner.Build = func(*Turn) (string, []string, error) { return "", nil, ErrNotFound }
	stop := runLoop(t, s)
	defer stop()

	turn, err := s.Submit("unbuildable", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	// Wait on the session, not the turn: the turn is finished before the session
	// degrades, so waiting on TurnFailed races.
	waitFor(t, "the session to go degraded", func() bool {
		return s.Status().State == StateDegraded
	})

	st := s.Status()
	if st.DegradedReason == "" {
		t.Error("degraded without a reason — the user cannot act on that")
	}
	got, err := s.Queue.Get(turn.ID)
	if err != nil || got.State != TurnFailed {
		t.Errorf("turn state = %v (err %v), want %s", got.State, err, TurnFailed)
	}
}

func TestDegradedIsStickyUntilExplicitlyCleared(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `printf 'ok'` })
	s.degrade("the approval hook stopped being accepted")

	if st := s.Status(); st.State != StateDegraded {
		t.Fatalf("state = %s, want degraded", st.State)
	}
	s.setState(StateIdle, "")
	if st := s.Status(); st.State != StateDegraded {
		t.Error("degraded was cleared by an ordinary idle transition")
	}
	s.Recovered()
	if st := s.Status(); st.State != StateIdle || st.DegradedReason != "" {
		t.Errorf("Recovered did not clear the degraded state: %+v", s.Status())
	}
}

func TestRepeatedDegradeIsJournaledOnce(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `true` })
	for i := 0; i < 5; i++ {
		s.degrade("same underlying problem")
	}
	var n int
	for _, ev := range collect(t, s.Journal, 0) {
		if ev.Type == EventSessionDegraded {
			n++
		}
	}
	if n != 1 {
		t.Errorf("journaled %d degraded events for one repeated reason, want 1", n)
	}

	s.degrade("a different problem")
	n = 0
	for _, ev := range collect(t, s.Journal, 0) {
		if ev.Type == EventSessionDegraded {
			n++
		}
	}
	if n != 2 {
		t.Errorf("a new reason was not journaled: %d events", n)
	}
}

func TestSnapshotIsRefusedWhileATurnIsRunning(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `sleep 5` })
	stop := runLoop(t, s)
	defer stop()

	if ok, _ := s.SafeToSnapshot(); !ok {
		t.Error("an idle workstation refused a snapshot")
	}

	turn, err := s.Submit("long task", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to start", func() bool { return s.Runner.Busy() })

	ok, reason := s.SafeToSnapshot()
	if ok {
		t.Error("snapshot allowed while a task was running")
	}
	if reason == "" {
		t.Error("refusal carries no reason, so a caller cannot explain the wait")
	}

	if err := s.Interrupt(turn.ID, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "snapshot to become safe after the interrupt", func() bool {
		ok, _ := s.SafeToSnapshot()
		return ok
	})
}

func TestInterruptThroughTheSupervisorMarksTheTurnInterrupted(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `printf 'started\n'; sleep 30` })
	stop := runLoop(t, s)
	defer stop()

	turn, err := s.Submit("stop me", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, s.Journal, "started")

	if err := s.Interrupt(turn.ID, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to be recorded as interrupted", func() bool {
		got, err := s.Queue.Get(turn.ID)
		return err == nil && got.State == TurnInterrupted
	})
	if got := outputOf(t, s.Journal, "stdout"); !strings.Contains(got, "started") {
		t.Errorf("partial output was lost: %q", got)
	}
}

func TestStatusReportsStateModeAndQueueDepth(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `sleep 3` })

	if st := s.Status(); st.State != StateStarting {
		t.Errorf("before the loop starts, state = %s, want %s", st.State, StateStarting)
	}

	stop := runLoop(t, s)
	defer stop()
	waitFor(t, "the session to become idle", func() bool { return s.Status().State == StateIdle })

	first, _ := s.Submit("one", ModeAutonomous, "latest", "api", false)
	waitFor(t, "the session to become busy", func() bool { return s.Status().State == StateBusy })

	if _, err := s.Submit("two", ModeAutonomous, "latest", "api", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the queued turn to be counted", func() bool { return s.Status().QueueDepth == 1 })

	st := s.Status()
	if st.Mode != ModeRich {
		t.Errorf("mode = %q, want %q", st.Mode, ModeRich)
	}
	if st.AgentVersion != "test-1.0" {
		t.Errorf("agent version = %q", st.AgentVersion)
	}
	if st.CurrentTurn == nil || st.CurrentTurn.ID != first.ID {
		t.Errorf("current turn = %+v, want %s", st.CurrentTurn, first.ID)
	}
	if st.LastSeq == 0 {
		t.Error("LastSeq is zero, so a client has no cursor to stream from")
	}
}

func TestSessionLifecycleIsJournaled(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `true` })
	stop := runLoop(t, s)
	waitFor(t, "the session to become idle", func() bool { return s.Status().State == StateIdle })
	stop()

	types := typesOf(t, s.Journal)
	if len(types) < 3 {
		t.Fatalf("events = %v, want starting/ready/stopped", types)
	}
	if types[0] != EventSessionStarting || types[1] != EventSessionReady {
		t.Errorf("first events = %v, want %s then %s", types[:2], EventSessionStarting, EventSessionReady)
	}
	if last := types[len(types)-1]; last != EventSessionStopped {
		t.Errorf("last event = %s, want %s", last, EventSessionStopped)
	}
}

func TestAgentRestartIsCountedAndExplained(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `true` })

	s.NoteAgentRestart("exit status 139")
	st := s.Status()
	if st.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", st.Restarts)
	}
	if !strings.Contains(st.DegradedReason, "restarted") {
		t.Errorf("reason %q does not say the agent was restarted", st.DegradedReason)
	}

	var found bool
	for _, ev := range collect(t, s.Journal, 0) {
		if ev.Type == EventSessionDegraded && strings.Contains(string(ev.Payload), "139") {
			found = true
		}
	}
	if !found {
		t.Error("the restart cause was not journaled, so it cannot be diagnosed later")
	}
}

func TestTurnsQueuedBeforeTheLoopStartsAreServed(t *testing.T) {
	s := newTestSupervisor(t, func(*Turn) string { return `printf 'served'` })

	turn, err := s.Submit("queued while stopped", ModeAutonomous, "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}

	stop := runLoop(t, s)
	defer stop()
	waitFor(t, "the pre-queued turn to run", func() bool {
		got, err := s.Queue.Get(turn.ID)
		return err == nil && got.State == TurnDone
	})
}

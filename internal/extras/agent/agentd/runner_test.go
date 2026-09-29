package agentd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestRunner wires a runner over a real journal and real processes, since
// signals and pipe draining are what these tests exercise.
func newTestRunner(t *testing.T, script func(*Turn) string) (*Runner, *Journal) {
	t.Helper()
	j := openTestJournal(t, t.TempDir())
	r := &Runner{
		Journal: j,
		Build: func(turn *Turn) (string, []string, error) {
			return script(turn), os.Environ(), nil
		},
		Grace: 500 * time.Millisecond,
	}
	return r, j
}

func turnFor(req string) *Turn {
	return &Turn{ID: NewTurnID(time.Now()), Request: req, State: TurnRunning, Mode: ModeAutonomous}
}

// outputOf concatenates the turn's journaled output for one stream.
func outputOf(t *testing.T, j *Journal, stream string) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range collect(t, j, 0) {
		if ev.Type != EventTurnOutput {
			continue
		}
		var p struct{ Text, Stream string }
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if stream == "" || p.Stream == stream {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// waitForOutput blocks until the journal contains substr. Tests synchronise on
// output, not on "the process started", since the shell may not have printed yet.
func waitForOutput(t *testing.T, j *Journal, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(outputOf(t, j, ""), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in the journal; got %q", substr, outputOf(t, j, ""))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func typesOf(t *testing.T, j *Journal) []string {
	t.Helper()
	var out []string
	for _, ev := range collect(t, j, 0) {
		out = append(out, ev.Type)
	}
	return out
}

func TestRunJournalsStartOutputAndFinish(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string { return `printf 'hello world'` })

	res, err := r.Run(context.Background(), turnFor("say hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 || res.Interrupted {
		t.Errorf("result = %+v, want a clean exit", res)
	}
	if got := outputOf(t, j, "stdout"); got != "hello world" {
		t.Errorf("stdout = %q, want %q", got, "hello world")
	}
	if res.Reply != "hello world" {
		t.Errorf("reply = %q", res.Reply)
	}
	types := typesOf(t, j)
	if types[0] != EventTurnStarted {
		t.Errorf("first event = %s, want %s", types[0], EventTurnStarted)
	}
	if last := types[len(types)-1]; last != EventTurnFinished {
		t.Errorf("last event = %s, want %s", last, EventTurnFinished)
	}
}

func TestStdoutAndStderrAreDistinguished(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string { return `printf 'out'; printf 'err' >&2` })

	if _, err := r.Run(context.Background(), turnFor("both streams")); err != nil {
		t.Fatal(err)
	}
	if got := outputOf(t, j, "stdout"); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := outputOf(t, j, "stderr"); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
}

func TestNonZeroExitIsRecordedNotAnError(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string { return `printf 'no key configured' >&2; exit 3` })

	res, err := r.Run(context.Background(), turnFor("fail"))
	if err != nil {
		t.Fatalf("a failing agent must not be an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	types := typesOf(t, j)
	if last := types[len(types)-1]; last != EventTurnFailed {
		t.Errorf("last event = %s, want %s", last, EventTurnFailed)
	}
	if got := outputOf(t, j, "stderr"); !strings.Contains(got, "no key configured") {
		t.Errorf("the agent's explanation was lost: %q", got)
	}
}

func TestInterruptStopsTheTurnAndKeepsPartialOutput(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string {
		return `printf 'partial work done\n'; sleep 30`
	})

	turn := turnFor("long task")
	done := make(chan Result, 1)
	go func() {
		res, err := r.Run(context.Background(), turn)
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()

	waitForOutput(t, j, "partial work done")
	if err := r.Interrupt(turn.ID, 200*time.Millisecond); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	select {
	case res := <-done:
		if !res.Interrupted {
			t.Error("result does not report an interrupt")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("interrupt did not stop the turn")
	}

	if got := outputOf(t, j, "stdout"); !strings.Contains(got, "partial work done") {
		t.Errorf("output produced before the interrupt was lost: %q", got)
	}
	types := typesOf(t, j)
	if last := types[len(types)-1]; last != EventTurnInterrupted {
		t.Errorf("last event = %s, want %s", last, EventTurnInterrupted)
	}
}

func TestInterruptKillsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/child-still-alive"

	r, _ := newTestRunner(t, func(*Turn) string {
		// A surviving grandchild keeps touching the marker.
		return `( while true; do touch ` + marker + `; sleep 0.1; done ) & sleep 30`
	})

	turn := turnFor("spawns children")
	go func() { _, _ = r.Run(context.Background(), turn) }()

	deadline := time.Now().Add(5 * time.Second)
	for r.RunningTurn() != turn.ID {
		if time.Now().After(deadline) {
			t.Fatal("turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if err := r.Interrupt(turn.ID, 200*time.Millisecond); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	// Let the SIGKILL escalation land.
	time.Sleep(1 * time.Second)

	_ = os.Remove(marker)
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child process outlived the interrupt and is still running")
	}
}

func TestWedgedAgentIsKilledAfterTheGracePeriod(t *testing.T) {
	// trap protects only the shell; a bare sleep 30 would die to the group signal
	// and the shell would exit, so loop over short sleeps instead.
	r, j := newTestRunner(t, func(*Turn) string {
		return `trap '' INT; printf 'ignoring interrupts\n'; while true; do sleep 0.2; done`
	})

	turn := turnFor("ignores SIGINT")
	done := make(chan Result, 1)
	go func() {
		res, _ := r.Run(context.Background(), turn)
		done <- res
	}()

	waitForOutput(t, j, "ignoring interrupts")

	start := time.Now()
	if err := r.Interrupt(turn.ID, 300*time.Millisecond); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Errorf("killed after %v — the grace period was not honoured", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a SIGINT-ignoring agent was never killed")
	}
}

func TestInterruptWithAStaleTurnIDIsRefused(t *testing.T) {
	r, _ := newTestRunner(t, func(*Turn) string { return `sleep 30` })

	turn := turnFor("current")
	go func() { _, _ = r.Run(context.Background(), turn) }()

	deadline := time.Now().Add(5 * time.Second)
	for r.RunningTurn() != turn.ID {
		if time.Now().After(deadline) {
			t.Fatal("turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.Interrupt("turn_someoldone", 0); err == nil {
		t.Error("an interrupt for a turn that is not running was accepted")
	}
	if r.RunningTurn() != turn.ID {
		t.Error("the running turn was killed by a stale interrupt")
	}
	_ = r.Interrupt(turn.ID, 100*time.Millisecond)
}

func TestInterruptWithNothingRunningReportsSo(t *testing.T) {
	r, _ := newTestRunner(t, func(*Turn) string { return `true` })
	if err := r.Interrupt("", 0); err != ErrNoTurnRunning {
		t.Errorf("interrupt with an idle runner = %v, want ErrNoTurnRunning", err)
	}
}

func TestTailOfOutputIsNotLostAtExit(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string {
		return `for i in $(seq 1 200); do printf 'line-%s\n' "$i"; done`
	})

	if _, err := r.Run(context.Background(), turnFor("chatty")); err != nil {
		t.Fatal(err)
	}
	got := outputOf(t, j, "stdout")
	if !strings.Contains(got, "line-200") {
		t.Error("the last line of output was dropped")
	}
	if !strings.Contains(got, "line-1\n") {
		t.Error("the first line of output was dropped")
	}
}

func TestOutputIsCoalescedIntoFewEvents(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string {
		return `for i in $(seq 1 400); do printf 'x'; done`
	})

	if _, err := r.Run(context.Background(), turnFor("many small writes")); err != nil {
		t.Fatal(err)
	}
	var outputEvents int
	for _, ty := range typesOf(t, j) {
		if ty == EventTurnOutput {
			outputEvents++
		}
	}
	if outputEvents > 20 {
		t.Errorf("400 small writes produced %d output events; coalescing is not working", outputEvents)
	}
	if got := outputOf(t, j, "stdout"); len(got) != 400 {
		t.Errorf("coalescing lost bytes: got %d, want 400", len(got))
	}
}

func TestCommandBuildFailureIsJournaled(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	r := &Runner{
		Journal: j,
		Build: func(*Turn) (string, []string, error) {
			return "", nil, ErrNotFound
		},
	}
	if _, err := r.Run(context.Background(), turnFor("unbuildable")); err == nil {
		t.Fatal("expected the build error to surface")
	}
	types := typesOf(t, j)
	if len(types) != 1 || types[0] != EventTurnFailed {
		t.Errorf("events = %v, want a single %s", types, EventTurnFailed)
	}
}

func TestContextCancellationStopsTheTurn(t *testing.T) {
	r, _ := newTestRunner(t, func(*Turn) string { return `sleep 30` })
	ctx, cancel := context.WithCancel(context.Background())

	turn := turnFor("cancelled")
	done := make(chan struct{})
	go func() {
		_, _ = r.Run(ctx, turn)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for r.RunningTurn() != turn.ID {
		if time.Now().After(deadline) {
			t.Fatal("turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("context cancellation did not stop the turn")
	}
	if r.Busy() {
		t.Error("runner still reports a live process after cancellation")
	}
}

func TestBusyReflectsWhetherAProcessIsLive(t *testing.T) {
	r, _ := newTestRunner(t, func(*Turn) string { return `sleep 5` })
	if r.Busy() {
		t.Error("idle runner reports busy")
	}

	turn := turnFor("work")
	go func() { _, _ = r.Run(context.Background(), turn) }()

	deadline := time.Now().Add(5 * time.Second)
	for !r.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("runner never reported busy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = r.Interrupt(turn.ID, 100*time.Millisecond)

	deadline = time.Now().Add(10 * time.Second)
	for r.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("runner still busy long after the interrupt")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReplyKeepsTheTailAndSaysItWasTrimmed(t *testing.T) {
	r, _ := newTestRunner(t, func(*Turn) string {
		// Well over maxReplyBytes.
		return `for i in $(seq 1 5000); do printf 'padding-padding-padding\n'; done; printf 'FINAL ANSWER\n'`
	})

	res, err := r.Run(context.Background(), turnFor("verbose"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Reply, "FINAL ANSWER") {
		t.Error("the reply dropped the end of the output, which is where the answer is")
	}
	if !strings.Contains(res.Reply, "trimmed") {
		t.Error("a trimmed reply does not say so")
	}
	if len(res.Reply) > maxReplyBytes+200 {
		t.Errorf("reply is %d bytes, well past the cap", len(res.Reply))
	}
}

// Fails against a read-driven flush, which would buffer a line until the next read.
func TestOutputIsVisibleBeforeTheTurnEnds(t *testing.T) {
	r, j := newTestRunner(t, func(*Turn) string {
		return `printf 'first line\n'; sleep 30`
	})

	turn := turnFor("prints then goes quiet")
	go func() { _, _ = r.Run(context.Background(), turn) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(outputOf(t, j, "stdout"), "first line") {
		if time.Now().After(deadline) {
			t.Fatal("output was still buffered while the turn ran; the live console would show nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Otherwise buffering was not exercised.
	if !r.Busy() {
		t.Fatal("the turn ended before the assertion, so buffering was not exercised")
	}
	_ = r.Interrupt(turn.ID, 100*time.Millisecond)
}

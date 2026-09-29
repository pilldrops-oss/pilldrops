package agentd

import (
	"strings"
	"testing"
)

// The dashboard sends no mode; without the default Claude Code runs with its own
// approval gate armed and refuses every write while reporting success.
func TestUnsetModeDefaultsToAutonomous(t *testing.T) {
	q, err := OpenQueue(t.TempDir())
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	turn, err := q.Enqueue("do something", "", "", "api", false)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if turn.Mode != ModeAutonomous {
		t.Fatalf("mode = %q, want %q — without it the agent refuses every action "+
			"while reporting success", turn.Mode, ModeAutonomous)
	}
}

func TestExplicitModeIsPreserved(t *testing.T) {
	q, err := OpenQueue(t.TempDir())
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	for _, mode := range []string{"supervised", "guided", ModeAutonomous} {
		turn, err := q.Enqueue("task", mode, "", "api", false)
		if err != nil {
			t.Fatalf("enqueue %s: %v", mode, err)
		}
		if turn.Mode != mode {
			t.Fatalf("mode %q was rewritten to %q", mode, turn.Mode)
		}
	}
}

func TestDefaultedModeGetsTheAutonomyFlags(t *testing.T) {
	tp := &Templates{
		RunCmd: `claude -p "{task}"`, CLIBin: "claude",
		RunFlags: "--dangerously-skip-permissions", RunFlagsEnv: "IS_SANDBOX=1",
	}
	cmd, _, err := tp.Build(&Turn{Request: "hello", Mode: ModeAutonomous})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("autonomy flag missing: %q", cmd)
	}
	// Claude Code refuses to skip permissions as root without IS_SANDBOX=1.
	if !strings.Contains(cmd, "IS_SANDBOX=1") {
		t.Fatalf("required env missing: %q", cmd)
	}
}

func TestTerminalFrameDoesNotDuplicateStreamedText(t *testing.T) {
	acc := &turnAccum{}
	answer := "The file was created and contains EDGE_TO_EDGE."
	acc.noteText(answer)

	if !acc.alreadyStreamed(answer) {
		t.Fatal("the streamed answer was not recognised, so it would be printed twice")
	}
	// The result frame is not required to be byte-identical.
	if !acc.alreadyStreamed("  " + answer + "\n") {
		t.Fatal("whitespace difference defeated the duplicate check")
	}
	if acc.alreadyStreamed("Something else entirely.") {
		t.Fatal("new terminal text was suppressed as a duplicate")
	}
}

func TestTerminalFrameIsKeptWhenNothingStreamed(t *testing.T) {
	acc := &turnAccum{}
	if acc.alreadyStreamed("the whole answer") {
		t.Fatal("suppressed the only copy of the answer")
	}
}

func TestStreamedTailIsBounded(t *testing.T) {
	acc := &turnAccum{}
	for range 40 {
		acc.noteText(strings.Repeat("x", 8<<10))
	}
	if len(acc.textTail) > streamedTailMax {
		t.Fatalf("tail grew to %d, over the %d cap", len(acc.textTail), streamedTailMax)
	}
	// Text longer than the retained tail cannot be judged, so it is shown.
	if acc.alreadyStreamed(strings.Repeat("y", streamedTailMax+1)) {
		t.Fatal("claimed to recognise text longer than the retained tail")
	}
}

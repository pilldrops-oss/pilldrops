package templates

import (
	"strings"
	"testing"
)

func TestAgentContinueCmdContract(t *testing.T) {
	var withContinue int
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.RunCmd == "" {
			continue
		}
		if !strings.Contains(a.RunCmd, "{task}") {
			t.Errorf("%s: RunCmd has no {task} placeholder: %q", tmpl.ID, a.RunCmd)
		}
		if a.ContinueCmd == "" {
			continue
		}
		withContinue++
		if !strings.Contains(a.ContinueCmd, "{task}") {
			t.Errorf("%s: ContinueCmd has no {task} placeholder: %q", tmpl.ID, a.ContinueCmd)
		}
		if strings.Contains(a.ContinueCmd, "{session}") {
			t.Errorf("%s: ContinueCmd must not need a session ID — the CLI tracks its own: %q",
				tmpl.ID, a.ContinueCmd)
		}
		if bin := a.CLIBin; bin != "" && !strings.HasPrefix(a.ContinueCmd, bin) {
			t.Errorf("%s: ContinueCmd %q does not invoke %q", tmpl.ID, a.ContinueCmd, bin)
		}
	}
	if withContinue == 0 {
		t.Error("no agent has a ContinueCmd — multi-turn chat would degrade to one-shot for every agent")
	}
}

func TestVerifiedAgentsSupportContinue(t *testing.T) {
	for _, id := range []string{"claude", "codex", "opencode"} {
		tmpl, ok := Resolve(id)
		if !ok {
			t.Fatalf("template %s missing", id)
		}
		if tmpl.Agent == nil || tmpl.Agent.ContinueCmd == "" {
			t.Errorf("%s lost its ContinueCmd — chat follow-ups would start a new conversation each time", id)
		}
	}
}

// grok's empty ContinueCmd is deliberate: its CLI rejects --continue and --session.
func TestGrokHasNoVerifiedContinue(t *testing.T) {
	tmpl, ok := Resolve("grok")
	if !ok {
		t.Fatal("template grok missing")
	}
	if tmpl.Agent == nil {
		t.Fatal("grok lost its AgentMeta")
	}
	if tmpl.Agent.ContinueCmd != "" {
		t.Errorf("grok has a ContinueCmd (%q), but its CLI rejects --continue and --session. "+
			"If a real resume flag shipped, verify it against the installed CLI and update the probe doc.",
			tmpl.Agent.ContinueCmd)
	}
}

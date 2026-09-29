package templates

import (
	"strings"
	"testing"
)

// knownStreamFormats are the event dialects a consumer can parse.
var knownStreamFormats = map[string]bool{
	"claude-stream-json": true,
	"codex-json":         true,
	"opencode-json":      true,
}

func TestStreamCmdAndFormatTravelTogether(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil {
			continue
		}
		switch {
		case a.StreamCmd != "" && a.StreamFormat == "":
			t.Errorf("%s: has StreamCmd but no StreamFormat — its events cannot be parsed", tmpl.ID)
		case a.StreamCmd == "" && a.StreamFormat != "":
			t.Errorf("%s: has StreamFormat %q but no StreamCmd — nothing produces those events",
				tmpl.ID, a.StreamFormat)
		}
		if a.StreamFormat != "" && !knownStreamFormats[a.StreamFormat] {
			t.Errorf("%s: StreamFormat %q has no parser; add one or leave the agent in plain-capture mode",
				tmpl.ID, a.StreamFormat)
		}
		if a.StreamCmd != "" {
			if !strings.Contains(a.StreamCmd, "{task}") {
				t.Errorf("%s: StreamCmd has no {task} placeholder: %q", tmpl.ID, a.StreamCmd)
			}
			if bin := a.CLIBin; bin != "" && !strings.HasPrefix(a.StreamCmd, bin) {
				t.Errorf("%s: StreamCmd %q does not invoke %q", tmpl.ID, a.StreamCmd, bin)
			}
		}
	}
}

func TestResumeCmdCarriesBothPlaceholders(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.ResumeCmd == "" {
			continue
		}
		for _, ph := range []string{"{session}", "{task}"} {
			if !strings.Contains(a.ResumeCmd, ph) {
				t.Errorf("%s: ResumeCmd has no %s placeholder: %q", tmpl.ID, ph, a.ResumeCmd)
			}
		}
		if bin := a.CLIBin; bin != "" && !strings.HasPrefix(a.ResumeCmd, bin) {
			t.Errorf("%s: ResumeCmd %q does not invoke %q", tmpl.ID, a.ResumeCmd, bin)
		}
	}
}

func TestPermissionToolCarriesToolPlaceholder(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.PermissionTool == "" {
			continue
		}
		if !strings.Contains(a.PermissionTool, "{tool}") {
			t.Errorf("%s: PermissionTool has no {tool} placeholder: %q", tmpl.ID, a.PermissionTool)
		}
		if !strings.HasPrefix(strings.TrimSpace(a.PermissionTool), "-") {
			t.Errorf("%s: PermissionTool should be a flag, got %q", tmpl.ID, a.PermissionTool)
		}
		// Without structured output there is no way to show what is being approved.
		if a.StreamCmd == "" {
			t.Errorf("%s: PermissionTool without StreamCmd — an approval with no visible tool call",
				tmpl.ID)
		}
	}
}

func TestCostSourceIsKnown(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil {
			continue
		}
		switch a.CostSource {
		case "", "stream":
		default:
			t.Errorf("%s: unknown CostSource %q (want \"stream\" or empty)", tmpl.ID, a.CostSource)
		}
		if a.CostSource == "stream" && a.StreamCmd == "" {
			t.Errorf("%s: CostSource is \"stream\" but there is no stream to read it from", tmpl.ID)
		}
	}
}

// Rich streaming does not imply a gate; codex exec rejects --ask-for-approval.
func TestCodexHasStreamingButNoApprovalHook(t *testing.T) {
	tmpl, ok := Resolve("codex")
	if !ok {
		t.Fatal("template codex missing")
	}
	a := tmpl.Agent
	if a == nil {
		t.Fatal("codex lost its AgentMeta")
	}
	if a.StreamCmd == "" {
		t.Error("codex lost its StreamCmd — verified emitting JSONL on 0.147.0")
	}
	if a.PermissionTool != "" {
		t.Errorf("codex has a PermissionTool (%q): `codex exec` rejects -a/--ask-for-approval. "+
			"If a real hook shipped, verify it end to end before offering Guided mode.",
			a.PermissionTool)
	}
}

func TestSomeAgentSupportsApprovalGating(t *testing.T) {
	for _, tmpl := range Catalog() {
		if tmpl.Agent != nil && tmpl.Agent.PermissionTool != "" {
			return
		}
	}
	t.Error("no agent declares a PermissionTool — Guided mode would have nothing to run on")
}

func TestModelsCmdOnlyWhereVerified(t *testing.T) {
	var withListing []string
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.ModelsCmd == "" {
			continue
		}
		withListing = append(withListing, tmpl.ID)
		if bin := a.CLIBin; bin != "" && !strings.HasPrefix(a.ModelsCmd, bin) {
			t.Errorf("%s: ModelsCmd %q does not invoke %q", tmpl.ID, a.ModelsCmd, bin)
		}
		if strings.Contains(a.ModelsCmd, "{task}") {
			t.Errorf("%s: ModelsCmd carries a {task} placeholder: %q", tmpl.ID, a.ModelsCmd)
		}
	}
	if len(withListing) != 1 || withListing[0] != "opencode" {
		t.Errorf("agents with ModelsCmd = %v; only opencode's listing has been verified. "+
			"If another agent gained one, verify it against the installed CLI first.", withListing)
	}
}

func TestAutonomyFlagsCarryTheEnvironmentTheyRequire(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.RunFlags == "" {
			continue
		}
		if tmpl.ID == "claude" {
			if a.RunFlagsEnv == "" {
				t.Errorf("claude lost RunFlagsEnv: --dangerously-skip-permissions is refused as root " +
					"without IS_SANDBOX=1, so every autonomous task would fail")
			}
			if !strings.Contains(a.RunFlagsEnv, "IS_SANDBOX=1") {
				t.Errorf("claude RunFlagsEnv = %q, want it to set IS_SANDBOX=1", a.RunFlagsEnv)
			}
		}
		if a.RunFlagsEnv != "" && !strings.Contains(a.RunFlagsEnv, "=") {
			t.Errorf("%s: RunFlagsEnv %q is not NAME=VALUE", tmpl.ID, a.RunFlagsEnv)
		}
	}
}

func TestAgentInstallCommandsArePinned(t *testing.T) {
	for _, tmpl := range Catalog() {
		a := tmpl.Agent
		if a == nil || a.InstallCmd == "" {
			continue
		}
		fields := strings.Fields(a.InstallCmd)
		spec := fields[len(fields)-1]
		name := spec
		if i := strings.LastIndex(spec, "@"); i > 0 {
			name = spec[:i]
		}
		if name == spec {
			t.Errorf("%s: InstallCmd %q is not pinned to a version — a rebake could change agent "+
				"underneath a customer, and every AgentMeta claim was observed on one version",
				tmpl.ID, a.InstallCmd)
		}
	}
}

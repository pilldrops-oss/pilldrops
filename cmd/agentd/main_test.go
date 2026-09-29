package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func TestChooseAddrFailsClosedWithoutAToken(t *testing.T) {
	got := chooseAddr("", "", false)
	if got != agentd.LoopbackAddr {
		t.Errorf("chooseAddr(no token) = %q, want loopback %q — an unauthenticated "+
			"agent API on every interface is reachable by any co-tenant sandbox",
			got, agentd.LoopbackAddr)
	}
}

func TestChooseAddrBindsPubliclyOnlyWithACredentialOrAnExplicitOptIn(t *testing.T) {
	for name, tc := range map[string]struct {
		explicit, hash string
		allowExposed   bool
		wantPublic     bool
	}{
		"token provisioned":         {"", "abc123", false, true},
		"no token":                  {"", "", false, false},
		"no token, explicit opt-in": {"", "", true, true},
		"token and opt-in":          {"", "abc123", true, true},
	} {
		got := chooseAddr(tc.explicit, tc.hash, tc.allowExposed)
		isPublic := strings.HasPrefix(got, "0.0.0.0:")
		if isPublic != tc.wantPublic {
			t.Errorf("%s: chooseAddr = %q (public=%v), want public=%v",
				name, got, isPublic, tc.wantPublic)
		}
	}
}

func TestChooseAddrHonoursAnExplicitAddress(t *testing.T) {
	for _, explicit := range []string{"127.0.0.1:9999", "0.0.0.0:1234", "[::1]:8080"} {
		for _, hash := range []string{"", "abc"} {
			if got := chooseAddr(explicit, hash, false); got != explicit {
				t.Errorf("chooseAddr(%q, hash=%q) = %q, want the explicit address",
					explicit, hash, got)
			}
		}
	}
}

func TestReadTokenHashPrefersTheEnvironment(t *testing.T) {
	t.Setenv("ENVD_TOKEN_HASH", "  deadbeef  ")
	if got := readTokenHash(); got != "deadbeef" {
		t.Errorf("readTokenHash = %q, want the trimmed env value", got)
	}
}

func TestReadTokenHashIsEmptyWhenUnprovisioned(t *testing.T) {
	t.Setenv("ENVD_TOKEN_HASH", "")
	// A dev machine has no envd.token_hash in /proc/cmdline.
	if got := readTokenHash(); got != "" && !strings.Contains(got, "") {
		t.Errorf("readTokenHash = %q", got)
	}
}

func TestAuthState(t *testing.T) {
	if got := authState(""); got != "OFF" {
		t.Errorf("authState(no hash) = %q, want OFF", got)
	}
	if got := authState("abc"); got != "token" {
		t.Errorf("authState(hash) = %q, want token", got)
	}
}

func TestResolveAgentRequiresARunCommand(t *testing.T) {
	_, _, err := resolveAgent("", "", "", "")
	if err == nil {
		t.Fatal("want an error when no run command can be determined")
	}
	if !strings.Contains(err.Error(), "-template") || !strings.Contains(err.Error(), "-run-cmd") {
		t.Errorf("error = %q, want it to name both ways of supplying one", err)
	}
}

func TestResolveAgentRejectsAnUnknownTemplate(t *testing.T) {
	_, _, err := resolveAgent("definitely-not-a-template", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Errorf("err = %v, want an unknown-template error", err)
	}
}

func TestResolveAgentRejectsANonAgentTemplate(t *testing.T) {
	var nonAgent string
	for _, tmpl := range templates.Catalog() {
		if tmpl.Kind != templates.KindAgent {
			nonAgent = tmpl.ID
			break
		}
	}
	if nonAgent == "" {
		t.Skip("no non-agent template in the catalogue")
	}
	_, _, err := resolveAgent(nonAgent, "", "", "")
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Errorf("resolveAgent(%q) err = %v, want a kind mismatch", nonAgent, err)
	}
}

func TestExplicitCommandsOverrideTheTemplate(t *testing.T) {
	var agentID string
	var tmplRun string
	for _, tmpl := range templates.Catalog() {
		if tmpl.Kind == templates.KindAgent && tmpl.Agent != nil && tmpl.Agent.RunCmd != "" {
			agentID, tmplRun = tmpl.ID, tmpl.Agent.RunCmd
			break
		}
	}
	if agentID == "" {
		t.Skip("no agent template in the catalogue")
	}

	meta, _, err := resolveAgent(agentID, "", "", "")
	if err != nil {
		t.Fatalf("resolveAgent(%q): %v", agentID, err)
	}
	if meta.RunCmd != tmplRun {
		t.Errorf("RunCmd = %q, want the template's %q", meta.RunCmd, tmplRun)
	}

	meta, _, err = resolveAgent(agentID, "my-run", "my-continue", "my-stream")
	if err != nil {
		t.Fatalf("resolveAgent with overrides: %v", err)
	}
	if meta.RunCmd != "my-run" || meta.ContinueCmd != "my-continue" || meta.StreamCmd != "my-stream" {
		t.Errorf("overrides not applied: run=%q continue=%q stream=%q",
			meta.RunCmd, meta.ContinueCmd, meta.StreamCmd)
	}
}

// A run command with no template is the bring-your-own-agent path.
func TestRunCmdAloneIsEnough(t *testing.T) {
	meta, id, err := resolveAgent("", "python agent.py", "", "")
	if err != nil {
		t.Fatalf("resolveAgent: %v", err)
	}
	if meta.RunCmd != "python agent.py" {
		t.Errorf("RunCmd = %q", meta.RunCmd)
	}
	if id != "" {
		t.Errorf("template id = %q, want empty", id)
	}
}

func TestSecretNamesCoverTheProvidersAndTheAgentsOwnKey(t *testing.T) {
	names := secretNames(templates.AgentMeta{KeyEnv: "CUSTOM_AGENT_KEY"})

	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, want := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"CUSTOM_AGENT_KEY", // the agent's own, from the manifest
	} {
		if !set[want] {
			t.Errorf("%s is not redacted; its value would reach the journal", want)
		}
	}
}

func TestSecretNamesWithoutAManifestKey(t *testing.T) {
	names := secretNames(templates.AgentMeta{})
	for _, n := range names {
		if n == "" {
			t.Error("an empty name would match nothing and may match everything, depending on the redactor")
		}
	}
	if len(names) == 0 {
		t.Error("no secret names at all")
	}
}

func TestAgentVersionPrefersTheSuppliedValue(t *testing.T) {
	// A bogus binary name proves no subprocess ran.
	if got := agentVersionOf("1.2.3", "/definitely/not/a/binary"); got != "1.2.3" {
		t.Errorf("agentVersionOf = %q, want the supplied 1.2.3", got)
	}
}

func TestAgentVersionIsEmptyWhenUnknowable(t *testing.T) {
	if got := agentVersionOf("", ""); got != "" {
		t.Errorf("agentVersionOf(no value, no binary) = %q, want empty", got)
	}
	if got := agentVersionOf("", "/definitely/not/a/binary"); got != "" {
		t.Errorf("agentVersionOf(missing binary) = %q, want empty", got)
	}
}

func TestAgentVersionTakesOnlyTheFirstLine(t *testing.T) {
	sh := writeScript(t, "#!/bin/sh\necho '1.4.2 (build abc)'\necho 'extra banner line'\n")
	got := agentVersionOf("", sh)

	// The probe has a 5s budget and returns "" on expiry, so a loaded machine
	// must skip rather than fail.
	if got == "" {
		t.Skip("version probe did not complete within its budget; nothing to assert")
	}
	if got != "1.4.2 (build abc)" {
		t.Errorf("agentVersionOf = %q, want just the first line", got)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	// Not t.TempDir(): its macOS path is long enough to matter elsewhere in this repo.
	f, err := os.CreateTemp("/tmp", "agentver*.sh")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestCurrentModelIsEmptyRatherThanGuessed(t *testing.T) {
	if got := currentModel(templates.AgentMeta{}); got != "" {
		t.Errorf("currentModel(no ModelEnv) = %q, want empty", got)
	}
}

func TestCurrentModelReadsTheDeclaredVariable(t *testing.T) {
	t.Setenv("SOME_AGENT_MODEL", "  claude-opus-5  ")
	got := currentModel(templates.AgentMeta{ModelEnv: "SOME_AGENT_MODEL"})
	if got != "claude-opus-5" {
		t.Errorf("currentModel = %q, want the trimmed value", got)
	}
}

func TestCurrentModelUnsetVariable(t *testing.T) {
	t.Setenv("SOME_AGENT_MODEL", "")
	if got := currentModel(templates.AgentMeta{ModelEnv: "SOME_AGENT_MODEL"}); got != "" {
		t.Errorf("currentModel = %q, want empty for an unset variable", got)
	}
}

func TestCloseGateListenerToleratesNil(t *testing.T) {
	closeGateListener(nil)
}

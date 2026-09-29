package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func expandToMap(t *testing.T, body string) map[string]any {
	t.Helper()
	out := expandAgentCreate([]byte(body))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("expand output not JSON: %v", err)
	}
	return obj
}

func TestExpandAgentCreateTaskMode(t *testing.T) {
	obj := expandToMap(t, `{"template":"claude","args":{"task":"add \"tests\" to src/","access_mode":"task"},"setup":["echo hi"]}`)

	if _, has := obj["args"]; has {
		t.Fatal("args not stripped")
	}
	if obj["start"] != "sh _pd_agent_start.sh" {
		t.Fatalf("start = %v", obj["start"])
	}
	// Read the install command from the manifest so a pin bump does not break the test.
	wantInstall := installCmdFor(t, "claude")
	setup, _ := obj["setup"].([]any)
	if len(setup) != 2 || setup[0] != wantInstall || setup[1] != "echo hi" {
		t.Fatalf("setup = %v (want [%q, \"echo hi\"]: install first, caller's lines kept)",
			setup, wantInstall)
	}
	files, _ := obj["files"].([]any)
	var paths []string
	var taskContent, script string
	for _, fa := range files {
		f := fa.(map[string]any)
		p, _ := f["path"].(string)
		paths = append(paths, p)
		if p == "_pd_task.txt" {
			taskContent, _ = f["content"].(string)
		}
		if p == "_pd_agent_start.sh" {
			script, _ = f["content"].(string)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("files = %v", paths)
	}
	// The task rides as a file, never spliced into the shell.
	if taskContent != `add "tests" to src/` {
		t.Fatalf("task content = %q", taskContent)
	}
	if strings.Contains(script, `add "tests"`) {
		t.Fatal("task text leaked into the script (shell-injection surface)")
	}
	if !strings.Contains(script, "_pd_task.txt") || !strings.Contains(script, "/tmp/agent.log") {
		t.Fatalf("script doesn't read the task file / serve the log:\n%s", script)
	}
	if obj["port"] != float64(8000) {
		t.Fatalf("port = %v", obj["port"])
	}
}

func TestExpandAgentCreateTerminalMode(t *testing.T) {
	// No task means terminal mode.
	obj := expandToMap(t, `{"template":"codex","args":{}}`)
	files, _ := obj["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	script, _ := files[0].(map[string]any)["content"].(string)
	if !strings.Contains(script, "ttyd") {
		t.Fatal("terminal mode should serve ttyd")
	}
	wantInstall := installCmdFor(t, "codex")
	setup, _ := obj["setup"].([]any)
	if len(setup) != 1 || setup[0] != wantInstall {
		t.Fatalf("setup = %v, want [%q]", setup, wantInstall)
	}
}

// installCmdFor reads an agent's install command from the launch manifest, the
// same source the expansion reads.
func installCmdFor(t *testing.T, id string) string {
	t.Helper()
	tmpl, ok := templates.Resolve(id)
	if !ok || tmpl.Agent == nil {
		t.Fatalf("template %s has no agent metadata", id)
	}
	if tmpl.Agent.InstallCmd == "" {
		t.Fatalf("template %s has no InstallCmd", id)
	}
	return tmpl.Agent.InstallCmd
}

func TestExpandAgentCreatePassthrough(t *testing.T) {
	// Non-agent template with args: untouched (not ours to interpret).
	in := `{"template":"loadtest","args":{"url":"https://x"}}`
	if out := string(expandAgentCreate([]byte(in))); out != in {
		t.Fatalf("non-agent body rewritten: %s", out)
	}
	// No args: untouched.
	in = `{"template":"claude","env":["A=b"]}`
	if out := string(expandAgentCreate([]byte(in))); out != in {
		t.Fatalf("args-less body rewritten: %s", out)
	}
	// Explicit start wins: args stripped, start preserved, no script injected.
	obj := expandToMap(t, `{"template":"claude","args":{"task":"x"},"start":"claude --version"}`)
	if obj["start"] != "claude --version" {
		t.Fatalf("start = %v", obj["start"])
	}
	if _, has := obj["files"]; has {
		t.Fatal("files injected despite explicit start")
	}
}

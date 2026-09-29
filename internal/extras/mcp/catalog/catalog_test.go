package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogEntriesAreUsable(t *testing.T) {
	cat := Catalog()
	if len(cat) < 8 {
		t.Fatalf("catalogue has %d entries, want a useful starting set", len(cat))
	}
	seen := map[string]bool{}
	for _, s := range cat {
		if s.ID == "" || s.Name == "" || s.Description == "" {
			t.Errorf("%+v is missing an id, name or description — it cannot be listed", s)
		}
		if seen[s.ID] {
			t.Errorf("duplicate id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Command == "" && s.URL == "" {
			t.Errorf("%q has no command and no url", s.ID)
		}
	}
}

func TestRenderClaude(t *testing.T) {
	path, content, err := Render("claude", []string{"github"}, map[string]string{"GITHUB_TOKEN": "ghp_x"})
	if err != nil {
		t.Fatal(err)
	}
	if path != ".mcp.json" {
		t.Errorf("path = %q, want .mcp.json", path)
	}
	var got struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(content), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, content)
	}
	gh, ok := got.MCPServers["github"]
	if !ok {
		t.Fatalf("no github entry in %s", content)
	}
	if gh.Command != "npx" || len(gh.Args) == 0 {
		t.Errorf("github entry = %+v, want an npx invocation", gh)
	}
	if gh.Env["GITHUB_TOKEN"] != "ghp_x" {
		t.Errorf("credential not written through: %+v", gh.Env)
	}
}

func TestRenderCodexIsToml(t *testing.T) {
	path, content, err := Render("codex", []string{"github"}, map[string]string{"GITHUB_TOKEN": "ghp_x"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "~/.codex/config.toml" {
		t.Errorf("path = %q, want the codex TOML path", path)
	}
	if !strings.Contains(content, "[mcp_servers.github]") {
		t.Errorf("no TOML table header in:\n%s", content)
	}
	if strings.HasPrefix(strings.TrimSpace(content), "{") {
		t.Error("codex config was rendered as JSON — the agent would ignore it")
	}
	if !strings.Contains(content, `GITHUB_TOKEN = "ghp_x"`) {
		t.Errorf("credential missing from TOML:\n%s", content)
	}
}

func TestRenderOpenCodeShape(t *testing.T) {
	_, content, err := Render("opencode", []string{"github"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(content), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	gh := got.MCP["github"]
	if gh.Type != "local" || len(gh.Command) < 2 || !gh.Enabled {
		t.Errorf("opencode entry = %+v, want a local, enabled, argv-style command", gh)
	}
}

func TestRenderAmpNamespacedKey(t *testing.T) {
	_, content, err := Render("amp", []string{"github"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"amp.mcpServers"`) {
		t.Errorf("amp config is not namespaced:\n%s", content)
	}
}

func TestRenderRemoteServer(t *testing.T) {
	_, content, err := Render("claude", []string{"linear"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "https://") {
		t.Errorf("remote server rendered without its url:\n%s", content)
	}
	if strings.Contains(content, `"command"`) {
		t.Errorf("remote server rendered as a local process:\n%s", content)
	}
}

func TestRenderRejectsUnknownInput(t *testing.T) {
	if _, _, err := Render("claude", []string{"not-a-real-server"}, nil); err == nil {
		t.Error("unknown server id was accepted — the config would be silently wrong")
	}
	if _, _, err := Render("no-such-agent", []string{"github"}, nil); err == nil {
		t.Error("unknown agent was accepted")
	}
	if _, _, err := Render("claude", nil, nil); err == nil {
		t.Error("empty selection was accepted — writing an empty config is not a no-op")
	}
}

func TestEveryKnownAgentRenders(t *testing.T) {
	for _, agent := range Agents() {
		path, content, err := Render(agent, []string{"github", "playwright"}, map[string]string{"GITHUB_TOKEN": "t"})
		if err != nil {
			t.Errorf("agent %q does not render: %v", agent, err)
			continue
		}
		if path == "" || content == "" {
			t.Errorf("agent %q rendered nothing", agent)
		}
		if !strings.Contains(content, "github") || !strings.Contains(content, "playwright") {
			t.Errorf("agent %q dropped a selected server:\n%s", agent, content)
		}
	}
}

func TestCredentialsGoOnlyToTheServerThatNeedsThem(t *testing.T) {
	_, content, err := Render("claude", []string{"github", "playwright"},
		map[string]string{"GITHUB_TOKEN": "ghp_secret"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(content), &got); err != nil {
		t.Fatal(err)
	}
	if got.MCPServers["github"].Env["GITHUB_TOKEN"] != "ghp_secret" {
		t.Error("github did not receive its own token")
	}
	if _, leaked := got.MCPServers["playwright"].Env["GITHUB_TOKEN"]; leaked {
		t.Error("GITHUB_TOKEN leaked into an unrelated server's env")
	}
}

// Project-scope .mcp.json stays "Pending approval" forever on a headless workstation.
func TestClaudeTargetsUserScope(t *testing.T) {
	path, merge := Target("claude")
	if path != "~/.claude.json" {
		t.Errorf("claude target = %q, want ~/.claude.json (project scope needs approval nobody can give)", path)
	}
	if !merge {
		t.Error("claude target must MERGE — that file holds userID and machineID, and overwriting it resets the install")
	}
	if _, m := Target("codex"); m {
		t.Error("codex config is ours alone; merging adds risk for no gain")
	}
}

func TestMergePreservesExistingUserConfig(t *testing.T) {
	existing := []byte(`{"userID":"abc123","machineID":"m1","projects":{"/workspace":{"x":1}}}`)
	out, err := Merge("claude", existing, []string{"github"}, map[string]string{"GITHUB_TOKEN": "t"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("merged output is not valid JSON: %v", err)
	}
	if got["userID"] != "abc123" || got["machineID"] != "m1" {
		t.Errorf("merge destroyed install state: %v", got)
	}
	if got["projects"] == nil {
		t.Error("merge dropped the projects map")
	}
	srv, _ := got["mcpServers"].(map[string]any)
	gh, _ := srv["github"].(map[string]any)
	if gh == nil {
		t.Fatalf("github not added: %v", got["mcpServers"])
	}
	if gh["type"] != "stdio" {
		t.Errorf("user-scope entry needs an explicit type, got %v", gh["type"])
	}
}

func TestMergeHandlesAbsentFile(t *testing.T) {
	for _, existing := range [][]byte{nil, []byte(""), []byte("   ")} {
		out, err := Merge("claude", existing, []string{"github"}, nil)
		if err != nil {
			t.Fatalf("merge into %q failed: %v", existing, err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Errorf("merge into %q produced invalid JSON: %v", existing, err)
		}
		if got["mcpServers"] == nil {
			t.Errorf("merge into %q added nothing", existing)
		}
	}
}

func TestMergeRefusesCorruptFile(t *testing.T) {
	if _, err := Merge("claude", []byte("{not json"), []string{"github"}, nil); err == nil {
		t.Error("merge into a corrupt file succeeded — it would have destroyed the original")
	}
}

func TestMergeReplacesOurServersOnly(t *testing.T) {
	existing := []byte(`{"userID":"u","mcpServers":{"github":{"type":"stdio"},"handrolled":{"type":"stdio"}}}`)
	out, err := Merge("claude", existing, []string{"playwright"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	json.Unmarshal([]byte(out), &got)
	if _, still := got.MCPServers["github"]; still {
		t.Error("a deselected catalogue server survived the merge")
	}
	if _, gone := got.MCPServers["handrolled"]; !gone {
		t.Error("merge deleted a server the user added themselves — we only own our own entries")
	}
}

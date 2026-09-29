//go:build !core

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntegrationsCatalogIsServed(t *testing.T) {
	srv, key, _, _ := newCronSandbox(t)

	code, out := do(t, "GET", srv+"/v1/integrations", key, "")
	if code != http.StatusOK {
		t.Fatalf("catalog = %d %v", code, out)
	}
	servers, _ := out["servers"].([]any)
	if len(servers) < 8 {
		t.Fatalf("catalog has %d servers, want a useful set", len(servers))
	}
	first := servers[0].(map[string]any)
	for _, f := range []string{"id", "name", "description"} {
		if first[f] == nil || first[f] == "" {
			t.Errorf("catalog entry missing %q: %v", f, first)
		}
	}
	if agents, _ := out["agents"].([]any); len(agents) == 0 {
		t.Error("catalog does not say which agents can be configured")
	}
}

// Enabling a server must write a config the agent can read into the machine.
func TestEnablingAnIntegrationWritesConfigIntoTheSandbox(t *testing.T) {
	srv, key, _, _, api := newCronSandboxWithAPI(t)

	code, out := do(t, "POST", srv+"/v1/sandboxes", key, `{"template":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("create claude workstation = %d %v", code, out)
	}
	sb, _ := out["id"].(string)

	code, out = do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", key,
		`{"servers":["github","playwright"],"env":{"GITHUB_TOKEN":"ghp_test"}}`)
	if code != http.StatusOK {
		t.Fatalf("enable = %d %v", code, out)
	}
	// User scope, not .mcp.json: project scope needs an interactive approval.
	if out["path"] != "~/.claude.json" {
		t.Errorf("path = %v, want ~/.claude.json (user scope loads without approval)", out["path"])
	}

	// Read it back out of the machine via $HOME, where the agent looks.
	raw := api.readAgentConfig(httptest.NewRequest("GET", "/", nil), sb, "~/.claude.json")
	if len(raw) == 0 {
		t.Fatal("config was not written into the sandbox")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config in the sandbox is not valid JSON: %v\n%s", err, raw)
	}
	if _, ok := cfg.MCPServers["github"]; !ok {
		t.Errorf("github missing from the written config: %s", raw)
	}
	if _, ok := cfg.MCPServers["playwright"]; !ok {
		t.Errorf("playwright missing from the written config: %s", raw)
	}
	if cfg.MCPServers["github"].Env["GITHUB_TOKEN"] != "ghp_test" {
		t.Error("credential did not reach the config")
	}
	if _, leaked := cfg.MCPServers["playwright"].Env["GITHUB_TOKEN"]; leaked {
		t.Error("token leaked into an unrelated server")
	}

	_, out = do(t, "GET", srv+"/v1/sandboxes/"+sb+"/integrations", key, "")
	got, _ := out["servers"].([]any)
	if len(got) != 2 {
		t.Errorf("enabled set = %v, want two servers", got)
	}
}

// Off means gone; a merge would keep what was just unticked.
func TestDisablingRemovesFromTheConfig(t *testing.T) {
	srv, key, _, _, api := newCronSandboxWithAPI(t)
	_, out := do(t, "POST", srv+"/v1/sandboxes", key, `{"template":"claude"}`)
	sb, _ := out["id"].(string)

	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", key, `{"servers":["github","playwright"]}`)
	do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", key, `{"servers":["playwright"]}`)

	raw := api.readAgentConfig(httptest.NewRequest("GET", "/", nil), sb, "~/.claude.json")
	if len(raw) == 0 {
		t.Fatal("config missing")
	}
	if strings.Contains(string(raw), "github") {
		t.Errorf("github survived being switched off:\n%s", raw)
	}
	if !strings.Contains(string(raw), "playwright") {
		t.Errorf("playwright was dropped:\n%s", raw)
	}
}

func TestIntegrationsRejectBadInput(t *testing.T) {
	srv, key, sb, st := newCronSandbox(t)

	// The base template ships no agent.
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", key, `{"servers":["github"]}`); code != http.StatusBadRequest {
		t.Error("enabling on a non-agent template did not 400")
	}
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", key, `{"servers":[]}`); code != http.StatusBadRequest {
		t.Error("empty selection did not 400")
	}
	_, otherKey, _ := st.CreateKey(t.Context(), "other", "org_other")
	if code, _ := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/integrations", otherKey, `{"servers":["github"]}`); code != http.StatusNotFound {
		t.Error("cross-tenant enable was not 404")
	}
}

// The filesystem API is workdir-relative, so "~/" paths must go through $HOME.
func TestHomeRelativeConfigPathsResolveToAbsolute(t *testing.T) {
	_, _, sb, _, api := newCronSandboxWithAPI(t)
	r := httptest.NewRequest("POST", "/", nil)

	if err := api.writeAgentConfig(r, sb, "~/.codex/config.toml", "hello-home"); err != nil {
		t.Fatalf("write ~/ config: %v", err)
	}
	if got := string(api.readAgentConfig(r, sb, "~/.codex/config.toml")); !strings.Contains(got, "hello-home") {
		t.Errorf("~/ config did not round-trip through $HOME, got %q", got)
	}

	if err := api.writeAgentConfig(r, sb, "opencode.json", "hello-workdir"); err != nil {
		t.Fatalf("write relative config: %v", err)
	}
	fs, _ := api.d.FS(sb)
	if b, err := fs.Read(t.Context(), "opencode.json"); err != nil || !strings.Contains(string(b), "hello-workdir") {
		t.Errorf("workdir-relative config not beside the project: %q %v", b, err)
	}
}

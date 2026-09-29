// Package catalog is a curated set of MCP servers that can be switched on for a
// workstation, rendered into whichever config file the chosen agent reads.
package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Server is one catalogue entry: a process to run (Command) or an endpoint to reach (URL).
type Server struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	// EnvKeys are the only credentials written into this server's config.
	EnvKeys []string `json:"env_keys,omitempty"`
}

// Every entry must actually install; a broken one looks like a broken agent.
var catalog = []Server{
	{ID: "github", Name: "GitHub", Category: "development",
		Description: "Read and write issues, pull requests and code across your repositories.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"},
		EnvKeys: []string{"GITHUB_TOKEN"}},

	{ID: "filesystem", Name: "Filesystem", Category: "development",
		Description: "Read and write files outside the agent's working directory.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"}},

	{ID: "postgres", Name: "Postgres", Category: "data",
		Description: "Query a Postgres database and inspect its schema.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-postgres"},
		EnvKeys: []string{"DATABASE_URL"}},

	{ID: "playwright", Name: "Playwright", Category: "testing",
		Description: "Drive a real browser: navigate, click, fill forms and screenshot.",
		Command:     "npx", Args: []string{"-y", "@playwright/mcp@latest"}},

	{ID: "slack", Name: "Slack", Category: "communication",
		Description: "Read channels and post messages as your bot.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-slack"},
		EnvKeys: []string{"SLACK_BOT_TOKEN", "SLACK_TEAM_ID"}},

	{ID: "notion", Name: "Notion", Category: "productivity",
		Description: "Search, read and update Notion pages and databases.",
		Command:     "npx", Args: []string{"-y", "@notionhq/notion-mcp-server"},
		EnvKeys: []string{"NOTION_TOKEN"}},

	{ID: "sentry", Name: "Sentry", Category: "observability",
		Description: "Pull issues and stack traces so the agent can debug from a real error.",
		Command:     "npx", Args: []string{"-y", "@sentry/mcp-server"},
		EnvKeys: []string{"SENTRY_AUTH_TOKEN"}},

	{ID: "brave-search", Name: "Brave Search", Category: "research",
		Description: "Search the web. The same capability a competitor meters at 25 calls a month.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-brave-search"},
		EnvKeys: []string{"BRAVE_API_KEY"}},

	{ID: "memory", Name: "Memory", Category: "productivity",
		Description: "A knowledge graph the agent keeps across conversations.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-memory"}},

	{ID: "sequential-thinking", Name: "Sequential Thinking", Category: "reasoning",
		Description: "Structured step-by-step reasoning for long problems.",
		Command:     "npx", Args: []string{"-y", "@modelcontextprotocol/server-sequential-thinking"}},

	{ID: "context7", Name: "Context7", Category: "development",
		Description: "Up-to-date library documentation, so the agent stops inventing APIs.",
		Command:     "npx", Args: []string{"-y", "@upstash/context7-mcp"}},

	// Keep at least one hosted entry so the URL render path stays exercised.
	{ID: "linear", Name: "Linear", Category: "productivity",
		Description: "Read and update Linear issues. Hosted — nothing to install.",
		URL:         "https://mcp.linear.app/mcp"},
}

// Catalog returns the available servers sorted by name.
func Catalog() []Server {
	out := append([]Server(nil), catalog...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ByID looks up one entry.
func ByID(id string) (Server, bool) {
	for _, s := range catalog {
		if s.ID == id {
			return s, true
		}
	}
	return Server{}, false
}

// Agents returns the agent template ids this package can render a config for.
func Agents() []string {
	var out []string
	for _, t := range templates.Catalog() {
		if t.Agent != nil && t.Agent.ConfigFile != "" {
			out = append(out, t.ID)
		}
	}
	sort.Strings(out)
	return out
}

// Render returns the config path an agent reads and its content enabling the
// selected servers. Each agent reads a different shape, and a wrong one is
// silently ignored rather than reported.
func Render(agent string, ids []string, env map[string]string) (path, content string, err error) {
	if len(ids) == 0 {
		return "", "", fmt.Errorf("select at least one server — writing an empty config is not the same as changing nothing")
	}
	tmpl, ok := templates.Resolve(agent)
	if !ok || tmpl.Agent == nil || tmpl.Agent.ConfigFile == "" {
		return "", "", fmt.Errorf("unknown agent %q", agent)
	}
	servers := make([]Server, 0, len(ids))
	for _, id := range ids {
		s, ok := ByID(id)
		if !ok {
			return "", "", fmt.Errorf("unknown server %q", id)
		}
		servers = append(servers, s)
	}

	switch agent {
	case "codex":
		content = renderTOML(servers, env)
	case "opencode":
		content, err = renderJSON(map[string]any{"mcp": opencodeEntries(servers, env)})
	case "amp":
		content, err = renderJSON(map[string]any{"amp.mcpServers": standardEntries(servers, env)})
	default:
		// claude and grok
		content, err = renderJSON(map[string]any{"mcpServers": standardEntries(servers, env)})
	}
	if err != nil {
		return "", "", err
	}
	return tmpl.Agent.ConfigFile, content, nil
}

// envFor returns only the credentials this server declares, so a token never
// reaches a server that has no business holding it.
func envFor(s Server, env map[string]string) map[string]string {
	if len(s.EnvKeys) == 0 || len(env) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, k := range s.EnvKeys {
		if v, ok := env[k]; ok && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// standardEntries is the {command,args,env} or {url} shape Claude, Grok and Amp accept.
func standardEntries(servers []Server, env map[string]string) map[string]any {
	out := map[string]any{}
	for _, s := range servers {
		if s.URL != "" {
			out[s.ID] = map[string]any{"type": "http", "url": s.URL}
			continue
		}
		e := map[string]any{"command": s.Command, "args": s.Args}
		if ev := envFor(s, env); ev != nil {
			e["env"] = ev
		}
		out[s.ID] = e
	}
	return out
}

// opencodeEntries is OpenCode's typed local/remote shape with the command as one argv array.
func opencodeEntries(servers []Server, env map[string]string) map[string]any {
	out := map[string]any{}
	for _, s := range servers {
		if s.URL != "" {
			out[s.ID] = map[string]any{"type": "remote", "url": s.URL, "enabled": true}
			continue
		}
		e := map[string]any{
			"type":    "local",
			"command": append([]string{s.Command}, s.Args...),
			"enabled": true,
		}
		if ev := envFor(s, env); ev != nil {
			e["environment"] = ev
		}
		out[s.ID] = e
	}
	return out
}

// renderTOML writes Codex's config by hand; the shape is three known forms.
func renderTOML(servers []Server, env map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by PillDrops — MCP servers for this workstation.\n")
	for _, s := range servers {
		fmt.Fprintf(&b, "\n[mcp_servers.%s]\n", s.ID)
		if s.URL != "" {
			fmt.Fprintf(&b, "url = %s\n", tomlString(s.URL))
			continue
		}
		fmt.Fprintf(&b, "command = %s\n", tomlString(s.Command))
		args := make([]string, 0, len(s.Args))
		for _, a := range s.Args {
			args = append(args, tomlString(a))
		}
		fmt.Fprintf(&b, "args = [%s]\n", strings.Join(args, ", "))
		if ev := envFor(s, env); ev != nil {
			keys := make([]string, 0, len(ev))
			for k := range ev {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				pairs = append(pairs, fmt.Sprintf("%s = %s", k, tomlString(ev[k])))
			}
			fmt.Fprintf(&b, "env = { %s }\n", strings.Join(pairs, ", "))
		}
	}
	return b.String()
}

// tomlString quotes a TOML basic string; a credential may contain quotes or backslashes.
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func renderJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// Target reports where an agent's MCP config belongs and whether to merge into it.
// Claude Code needs interactive approval for project-scope .mcp.json, so its
// servers go in user-scope ~/.claude.json, which also holds install state.
func Target(agent string) (path string, merge bool) {
	if agent == "claude" {
		return "~/.claude.json", true
	}
	tmpl, ok := templates.Resolve(agent)
	if !ok || tmpl.Agent == nil {
		return "", false
	}
	return tmpl.Agent.ConfigFile, false
}

// Merge folds the selected servers into an existing JSON config. Only catalogue
// ids are owned: hand-added servers survive, deselected catalogue ones are removed.
func Merge(agent string, existing []byte, ids []string, env map[string]string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("select at least one server")
	}
	doc := map[string]any{}
	if s := strings.TrimSpace(string(existing)); s != "" {
		if err := json.Unmarshal([]byte(s), &doc); err != nil {
			return "", fmt.Errorf("existing config is not valid JSON, refusing to overwrite it: %w", err)
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	for _, s := range catalog {
		delete(servers, s.ID)
	}
	for _, id := range ids {
		s, ok := ByID(id)
		if !ok {
			return "", fmt.Errorf("unknown server %q", id)
		}
		if s.URL != "" {
			servers[s.ID] = map[string]any{"type": "http", "url": s.URL}
			continue
		}
		// User scope needs an explicit transport type.
		e := map[string]any{"type": "stdio", "command": s.Command, "args": s.Args}
		if ev := envFor(s, env); ev != nil {
			e["env"] = ev
		}
		servers[s.ID] = e
	}
	doc["mcpServers"] = servers
	return renderJSON(doc)
}

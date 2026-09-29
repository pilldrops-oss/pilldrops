package templates

// Template kinds drive the launch form, the CTA and the post-launch workspace.
const (
	KindShell   = "shell"
	KindCode    = "code"
	KindApp     = "app"
	KindAgent   = "agent"
	KindScreen  = "screen"
	KindTest    = "test"
	KindVerdict = "verdict"
	KindService = "service"
)

// Kinds lists every valid kind.
var Kinds = []string{
	KindShell, KindCode, KindApp, KindAgent, KindScreen, KindTest, KindVerdict, KindService,
}

// PrimaryInput is the one thing a template's quick-launch asks for; nil means
// a zero-field launch. Type is one of code, url, target, apk, script, tests, task.
type PrimaryInput struct {
	Type     string `json:"type"`
	Label    string `json:"label"`
	Required bool   `json:"required,omitempty"`
}

// AgentMeta is the per-agent-CLI launch knowledge an agent template ships.
// Every command field is empty unless the flag was verified against the pinned
// version; callers must fall back rather than guess, since a wrong flag
// silently starts a new conversation or runs ungated.
type AgentMeta struct {
	// KeyEnv is empty for provider-agnostic agents; KeyNote then guides the user.
	KeyEnv   string `json:"key_env,omitempty"`
	KeyLabel string `json:"key_label,omitempty"`
	KeyNote  string `json:"key_note,omitempty"`
	// BaseImage is the image InstallCmd runs on; imageFor reads it too.
	BaseImage string `json:"base_image,omitempty"`
	// RunCmd runs a one-shot task; {task} is substituted.
	RunCmd string `json:"run_cmd,omitempty"`
	// ContinueCmd runs {task} as a follow-up turn in the most recent conversation.
	ContinueCmd     string `json:"continue_cmd,omitempty"`
	TaskPlaceholder string `json:"task_placeholder,omitempty"`
	CLIBin          string `json:"cli_bin,omitempty"`
	// InstallCmd is pinned so the capability claims in this struct stay falsifiable.
	InstallCmd string `json:"install_cmd,omitempty"`
	RunFlags   string `json:"run_flags,omitempty"`
	// RunFlagsEnv is "NAME=VALUE" environment RunFlags requires, applied only alongside it.
	RunFlagsEnv string `json:"run_flags_env,omitempty"`
	// StreamCmd runs {task} with structured event output on stdout.
	StreamCmd string `json:"stream_cmd,omitempty"`
	// StreamContinueCmd continues the latest conversation with structured
	// output. Two verified flags do not make a verified combination, so it is
	// set only where the pair was checked together.
	StreamContinueCmd string `json:"stream_continue_cmd,omitempty"`
	// StreamFormat names the event dialect; the dialects share no first frame,
	// and codex interleaves plain-text lines a parser must skip.
	StreamFormat string `json:"stream_format,omitempty"`
	// PermissionTool routes tool-approval decisions to a tool we serve; {tool} is the name.
	PermissionTool string `json:"permission_tool,omitempty"`
	// ResumeCmd resumes a specific conversation; {session} and {task} are substituted.
	ResumeCmd string `json:"resume_cmd,omitempty"`
	// ModelsCmd lists models one per line. Empty must render as "not supported", not an empty list.
	ModelsCmd string `json:"models_cmd,omitempty"`
	// CostSource is "stream" when events carry cost; empty must render as "not reported", not zero.
	CostSource string `json:"cost_source,omitempty"`

	// BaseURLEnv and ModelEnv are empty for agents that keep these in their config file.
	BaseURLEnv string `json:"base_url_env,omitempty"`
	ModelEnv   string `json:"model_env,omitempty"`
	// ConfigExample must be schema-correct; an example that does not load is worse than none.
	ConfigFile    string `json:"config_file,omitempty"`
	ConfigLabel   string `json:"config_label,omitempty"`
	ConfigFormat  string `json:"config_format,omitempty"`
	ConfigExample string `json:"config_example,omitempty"`
}

// launchManifests assigns kind, outcome and primary input per catalog id.
// Outcome is one of terminal, preview, screen, report, verdict, endpoint, connection.
var launchManifests = map[string]struct {
	Kind    string
	Outcome string
	Primary *PrimaryInput
}{
	"base": {Kind: KindShell, Outcome: "terminal"},

	// A shell whose point is the container daemon already running in it; the
	// preview shows whatever the user publishes on 8000.
	"docker": {Kind: KindShell, Outcome: "terminal"},

	// Services land on a host:port, not a page, so "endpoint" rather than
	// "preview". The password is collected as Required env, not PrimaryInput.
	"postgres": {Kind: KindService, Outcome: "endpoint"},
	"redis":    {Kind: KindService, Outcome: "endpoint"},
	"mysql":    {Kind: KindService, Outcome: "endpoint"},
	"mongodb":  {Kind: KindService, Outcome: "endpoint"},
	"kafka":    {Kind: KindService, Outcome: "endpoint"},
	"code-interpreter": {Kind: KindCode, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "code", Label: "Code or data — optional, opens a REPL without it"}},
	"web-scraper": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "task", Label: "What to scrape — ✨ AI Configure writes the scraper (demo starter included)"}},
	"webhook-inbox": {Kind: KindApp, Outcome: "preview"},
	"research": {Kind: KindCode, Outcome: "preview",
		Primary: &PrimaryInput{Type: "task", Label: "Research question — ✨ AI Configure turns it into a runnable plan"}},
	"fastapi": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, a working starter is included"}},
	"node": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, a working starter is included"}},
	"nextjs": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, the baked starter serves instantly"}},
	"go": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project — optional, a working starter is included"}},
	"rust": {Kind: KindApp, Outcome: "preview",
		Primary: &PrimaryInput{Type: "code", Label: "Project (cargo)"}},
	"openclaw": {Kind: KindApp, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "script", Label: "Browser-automation script", Required: true}},
	"browser": {Kind: KindScreen, Outcome: "screen",
		Primary: &PrimaryInput{Type: "url", Label: "Start URL — optional"}},
	"desktop": {Kind: KindScreen, Outcome: "screen",
		Primary: &PrimaryInput{Type: "url", Label: "Start URL — optional"}},
	"android": {Kind: KindScreen, Outcome: "screen",
		Primary: &PrimaryInput{Type: "apk", Label: "APK — optional, installs on boot"}},
	"android-headless": {Kind: KindTest, Outcome: "endpoint",
		Primary: &PrimaryInput{Type: "apk", Label: "APK — optional, pre-installed for your suite"}},
	"mobile-test": {Kind: KindVerdict, Outcome: "verdict",
		Primary: &PrimaryInput{Type: "script", Label: "Test script (run.sh)", Required: true}},
	"playwright": {Kind: KindTest, Outcome: "report",
		Primary: &PrimaryInput{Type: "tests", Label: "Tests — repo, zip or a spec file (demo suite included)"}},
	"security": {Kind: KindVerdict, Outcome: "verdict",
		Primary: &PrimaryInput{Type: "target", Label: "Code or URL to scan", Required: true}},
	"loadtest": {Kind: KindVerdict, Outcome: "verdict",
		Primary: &PrimaryInput{Type: "url", Label: "Target URL", Required: true}},
	"claude": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"codex": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"grok": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"opencode": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"openclaw-agent": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"hermes": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
	"amp": {Kind: KindAgent, Outcome: "terminal",
		Primary: &PrimaryInput{Type: "task", Label: "Task — optional, blank opens the agent's terminal"}},
}

// agentMeta holds the per-agent-CLI launch knowledge.
var agentMeta = map[string]AgentMeta{
	"claude": {
		BaseImage: "node:20-slim",
		KeyEnv:    "ANTHROPIC_API_KEY", KeyLabel: "Anthropic API key",
		RunCmd: `claude -p "{task}"`, TaskPlaceholder: "e.g. Clone the repo, add tests to src/, and open a PR",
		ContinueCmd: `claude -p --continue "{task}"`,
		CLIBin:      "claude", InstallCmd: "npm i -g @anthropic-ai/claude-code@2.1.197",
		// Claude refuses --dangerously-skip-permissions as root without IS_SANDBOX=1.
		RunFlags:    "--dangerously-skip-permissions",
		RunFlagsEnv: "IS_SANDBOX=1",
		// The first frame is a system/init object carrying model, tools and permissionMode.
		StreamCmd:    `claude -p --output-format stream-json --verbose "{task}"`,
		StreamFormat: "claude-stream-json",
		// Verified live: --continue composes with stream-json and keeps the session_id.
		StreamContinueCmd: `claude -p --continue --output-format stream-json --verbose "{task}"`,
		// Undocumented flag; probe it at session start and hide Guided mode if the probe fails.
		PermissionTool: "--permission-prompt-tool {tool}",
		ResumeCmd:      `claude -p --resume {session} "{task}"`,
		CostSource:     "stream",
		BaseURLEnv:     "ANTHROPIC_BASE_URL", ModelEnv: "ANTHROPIC_MODEL",
		ConfigFile: ".mcp.json", ConfigLabel: "MCP servers", ConfigFormat: "json",
		ConfigExample: `{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" }
    },
    "linear": { "type": "http", "url": "https://mcp.linear.app/mcp" }
  }
}`,
	},
	"codex": {
		BaseImage: "node:20-slim",
		KeyEnv:    "OPENAI_API_KEY", KeyLabel: "OpenAI API key",
		// codex exec refuses to run outside a git repo without --skip-git-repo-check.
		RunCmd: `codex exec --skip-git-repo-check "{task}"`, TaskPlaceholder: "e.g. Add a REST endpoint and its tests",
		ContinueCmd: `codex exec resume --last --skip-git-repo-check "{task}"`,
		CLIBin:      "codex", InstallCmd: "npm i -g @openai/codex@0.147.0",
		// The JSONL stream interleaves plain-text ERROR lines a parser must skip.
		StreamCmd:    `codex exec --json --skip-git-repo-check "{task}"`,
		StreamFormat: "codex-json",
		ResumeCmd:    `codex exec resume {session} --skip-git-repo-check "{task}"`,
		CostSource:   "stream",
		// No PermissionTool: codex exec rejects --ask-for-approval, so autonomy stays binary.
		RunFlags:   "--dangerously-bypass-approvals-and-sandbox",
		ConfigFile: "~/.codex/config.toml", ConfigLabel: "Config — model, MCP servers (TOML)", ConfigFormat: "toml",
		ConfigExample: `# ~/.codex/config.toml
model = "gpt-5.4"

[mcp_servers.github]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
env = { GITHUB_TOKEN = "${GITHUB_TOKEN}" }`,
	},
	"grok": {
		BaseImage: "node:20-slim",
		KeyEnv:    "GROK_API_KEY", KeyLabel: "Grok (xAI) API key",
		RunCmd: `grok -p "{task}"`, TaskPlaceholder: "e.g. Summarize this codebase and suggest refactors",
		CLIBin: "grok", InstallCmd: "npm i -g @vibe-kit/grok-cli@0.0.34",
		// No ContinueCmd or StreamCmd: grok rejects --continue and --session and
		// has no structured output, so it cannot hold a conversation at all.
		BaseURLEnv: "GROK_BASE_URL",
		ConfigFile: ".grok/settings.json", ConfigLabel: "Config — model & MCP servers", ConfigFormat: "json",
		ConfigExample: `{
  "defaultModel": "grok-code-fast-1",
  "mcpServers": {
    "linear": {
      "name": "linear",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@linear/mcp-server"]
    }
  }
}`,
	},
	"opencode": {
		BaseImage: "node:20-slim",
		KeyLabel:  "Provider API key",
		KeyNote:   "OpenCode is provider-agnostic — add your provider key (ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY, …) as an environment variable, or run `opencode auth login` in the terminal.",
		RunCmd:    `opencode run "{task}"`, TaskPlaceholder: "e.g. Implement the TODOs in main.py",
		ContinueCmd: `opencode run -c "{task}"`,
		// Works without a key.
		ModelsCmd: "opencode models",
		// Pinned: agentdockerfile_test.go compares this against the on-disk Dockerfile.
		CLIBin: "opencode", InstallCmd: "npm i -g opencode-ai@1.18.15",
		StreamCmd:    `opencode run --format json "{task}"`,
		StreamFormat: "opencode-json",
		ResumeCmd:    `opencode run -s {session} "{task}"`,
		// No CostSource: usage comes from the separate `opencode stats`, not the run events.
		ConfigFile: "opencode.json", ConfigLabel: "Config — model & MCP servers", ConfigFormat: "json",
		ConfigExample: `{
  "$schema": "https://opencode.ai/config.json",
  "model": "anthropic/claude-sonnet-4-5",
  "mcp": {
    "github": {
      "type": "local",
      "command": ["npx", "-y", "@modelcontextprotocol/server-github"],
      "enabled": true
    },
    "linear": { "type": "remote", "url": "https://mcp.linear.app/mcp", "enabled": true }
  }
}`,
	},
	"openclaw-agent": {
		BaseImage: "node:24-slim",
		KeyLabel:  "Provider API key",
		KeyNote: "OpenClaw is provider-agnostic — set your provider key (OPENAI_API_KEY, " +
			"ANTHROPIC_API_KEY, OPENROUTER_API_KEY, …) as an environment variable. For channels " +
			"and multi-agent routing run `openclaw onboard` in the terminal.",
		// The CLI requires a session selector; a fixed key puts every turn in
		// the same conversation, so no ContinueCmd is needed.
		RunCmd:          `openclaw agent --local --session-key pilldrops -m "{task}"`,
		TaskPlaceholder: "e.g. Summarize the repo and open a PR with the fixes",
		ResumeCmd:       `openclaw agent --local --session-id {session} -m "{task}"`,
		CLIBin:          "openclaw", InstallCmd: "npm i -g openclaw@2026.7.1-2",
		// No StreamCmd: --json prints one result object, not an event stream.
		// The config shape is mcp.servers, not mcpServers.
		ConfigFile: "~/.openclaw/openclaw.json", ConfigFormat: "json",
		ConfigLabel: "Config — MCP servers, agents, channels",
		ConfigExample: `{
  "mcp": {
    "servers": {
      "github": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-github"],
        "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" }
      }
    }
  }
}`,
	},
	"hermes": {
		BaseImage: "node:24-slim",
		KeyLabel:  "Provider API key",
		KeyNote: "Hermes is provider-agnostic — set your provider key (OPENROUTER_API_KEY, " +
			"OPENAI_API_KEY, ANTHROPIC_API_KEY, …) as an environment variable, or run `hermes model` " +
			"in the terminal to choose a provider and model.",
		RunCmd: `hermes -z "{task}"`, TaskPlaceholder: "e.g. Add tests for the auth middleware",
		// --continue parses and reaches the agent; that it restores context is unconfirmed.
		ContinueCmd: `hermes --continue -z "{task}"`,
		ResumeCmd:   `hermes --resume {session} -z "{task}"`,
		CLIBin:      "hermes", InstallCmd: "npm i -g hermes-agent@0.20.0",
		RunFlags: "--yolo",
		// No StreamCmd: --usage-file writes a summary after the run, not a stream.
		// No ConfigExample: the schema was never observed, config.yaml is created lazily.
		ConfigFile: "~/.hermes/config.yaml", ConfigFormat: "yaml",
		ConfigLabel: "Config — provider, model & MCP servers (YAML); secrets live in ~/.hermes/.env",
	},
	"amp": {
		BaseImage: "node:20-slim",
		KeyEnv:    "AMP_API_KEY", KeyLabel: "Amp API key",
		RunCmd: `amp -x "{task}"`, TaskPlaceholder: "e.g. Fix the failing tests in this repo",
		CLIBin: "amp", InstallCmd: "npm i -g @sourcegraph/amp@0.0.1786119357-g68379f",
		// No ContinueCmd: `amp threads continue --last -x` parses but resuming
		// is unconfirmed. No StreamCmd: `threads export` is post-hoc.
		ConfigFile: "~/.config/amp/settings.json", ConfigLabel: "MCP servers", ConfigFormat: "json",
		ConfigExample: `{
  "amp.mcpServers": {
    "playwright": { "command": "npx", "args": ["-y", "@playwright/mcp@latest"] },
    "linear": { "url": "https://mcp.linear.app/mcp" }
  }
}`,
	},
}

// AgentMetaByID returns a copy of the agent registry.
func AgentMetaByID() map[string]AgentMeta {
	out := make(map[string]AgentMeta, len(agentMeta))
	for id, m := range agentMeta {
		out[id] = m
	}
	return out
}

// AgentMetaFor looks up one agent without copying the whole registry.
func AgentMetaFor(id string) (AgentMeta, bool) {
	m, ok := agentMeta[id]
	return m, ok
}

// init decorates the compiled-in catalog with its launch manifests.
func init() {
	for i := range catalog {
		t := &catalog[i]
		if m, ok := launchManifests[t.ID]; ok {
			t.Kind = m.Kind
			t.Outcome = m.Outcome
			t.PrimaryInput = m.Primary
		}
		if a, ok := agentMeta[t.ID]; ok {
			am := a
			t.Agent = &am
		}
	}
}

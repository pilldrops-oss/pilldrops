
// AgentConfig is the per-agent launch metadata. The server manifest
// (template.agent) is authoritative; AGENT_CONFIG is only a fallback for
// servers without it, so add new agents to the manifest, not here.
export interface AgentConfig {
  // Empty when the agent is provider-agnostic; keyNote then guides the user instead.
  keyEnv: string
  keyLabel: string
  keyNote?: string
  runCmd: string
  taskPlaceholder: string
  // Injected only when non-empty; agents that keep these in their config file leave them blank.
  baseUrlEnv: string
  modelEnv: string
  // configExample is written verbatim to mcpFile, so it must match that file's real format.
  mcpFile: string
  configLabel: string
  configFormat: 'json' | 'toml'
  configExample: string
  installCmd: string
  cliBin: string
  runFlags: string
  docsUrl: string
}

export const AGENT_CONFIG: Record<string, AgentConfig> = {
  claude: {
    keyEnv: 'ANTHROPIC_API_KEY',
    keyLabel: 'Anthropic API key',
    runCmd: 'claude -p "{task}"',
    taskPlaceholder: 'e.g. Clone the repo, add tests to src/, and open a PR',
    baseUrlEnv: 'ANTHROPIC_BASE_URL',
    modelEnv: 'ANTHROPIC_MODEL',
    mcpFile: '.mcp.json',
    configLabel: 'MCP servers',
    configFormat: 'json',
    configExample: `{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "\${GITHUB_TOKEN}" }
    },
    "linear": { "type": "http", "url": "https://mcp.linear.app/mcp" }
  }
}`,
    installCmd: 'npm i -g @anthropic-ai/claude-code@2.1.197',
    cliBin: 'claude',
    runFlags: '--dangerously-skip-permissions',
    docsUrl: 'https://code.claude.com/docs/en/mcp',
  },
  codex: {
    keyEnv: 'OPENAI_API_KEY',
    keyLabel: 'OpenAI API key',
    runCmd: 'codex exec --skip-git-repo-check "{task}"',
    taskPlaceholder: 'e.g. Add a REST endpoint and its tests',
    baseUrlEnv: '',
    modelEnv: '',
    mcpFile: '~/.codex/config.toml',
    configLabel: 'Config — model, MCP servers (TOML)',
    configFormat: 'toml',
    configExample: `# ~/.codex/config.toml
model = "gpt-5.4"

[mcp_servers.github]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
env = { GITHUB_TOKEN = "\${GITHUB_TOKEN}" }`,
    installCmd: 'npm i -g @openai/codex@0.147.0',
    cliBin: 'codex',
    runFlags: '--dangerously-bypass-approvals-and-sandbox',
    docsUrl: 'https://github.com/openai/codex',
  },
  // Grok reads GROK_API_KEY, not XAI_API_KEY.
  grok: {
    keyEnv: 'GROK_API_KEY',
    keyLabel: 'Grok (xAI) API key',
    runCmd: 'grok -p "{task}"',
    taskPlaceholder: 'e.g. Summarize this codebase and suggest refactors',
    baseUrlEnv: 'GROK_BASE_URL',
    modelEnv: '',
    mcpFile: '.grok/settings.json',
    configLabel: 'Config — model & MCP servers',
    configFormat: 'json',
    configExample: `{
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
    installCmd: 'npm i -g @vibe-kit/grok-cli@0.0.34',
    cliBin: 'grok',
    runFlags: '',
    docsUrl: 'https://github.com/superagent-ai/grok-cli',
  },
  opencode: {
    keyEnv: '',
    keyLabel: 'Provider API key',
    keyNote:
      'OpenCode is provider-agnostic — add your provider key (ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY, …) in the Environment tab, or run `opencode auth login` in the terminal.',
    runCmd: 'opencode run "{task}"',
    taskPlaceholder: 'e.g. Implement the TODOs in main.py',
    baseUrlEnv: '',
    modelEnv: '',
    mcpFile: 'opencode.json',
    configLabel: 'Config — model & MCP servers',
    configFormat: 'json',
    configExample: `{
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
    installCmd: 'npm i -g opencode-ai@1.18.15',
    cliBin: 'opencode',
    runFlags: '',
    docsUrl: 'https://opencode.ai/docs/mcp-servers/',
  },
  hermes: {
    keyEnv: '',
    keyLabel: 'Provider API key',
    keyNote:
      'Hermes is provider-agnostic — add your provider key (OPENAI_API_KEY, ANTHROPIC_API_KEY, …) in the Environment tab, or run `hermes model` in the terminal to choose a provider and model.',
    runCmd: 'hermes -z "{task}"',
    taskPlaceholder: 'e.g. Add tests for the payments module',
    baseUrlEnv: '',
    modelEnv: '',
    mcpFile: '',
    configLabel: '',
    configFormat: 'json',
    configExample: '',
    installCmd: 'npm i -g hermes-agent@0.20.0',
    cliBin: 'hermes',
    runFlags: '-z',
    docsUrl: 'https://www.npmjs.com/package/hermes-agent',
  },
  'openclaw-agent': {
    keyEnv: '',
    keyLabel: 'Provider API key',
    keyNote:
      'OpenClaw is provider-agnostic — add your provider key (ANTHROPIC_API_KEY, OPENROUTER_API_KEY, …) in the Environment tab. For channels and multi-agent routing run `openclaw onboard` in the terminal.',
    runCmd: 'openclaw agent --local -m "{task}"',
    taskPlaceholder: 'e.g. Summarise today\'s CI failures',
    baseUrlEnv: '',
    modelEnv: '',
    mcpFile: '',
    configLabel: '',
    configFormat: 'json',
    configExample: '',
    installCmd: 'npm i -g openclaw@2026.7.1-2',
    cliBin: 'openclaw',
    runFlags: '--local',
    docsUrl: 'https://www.npmjs.com/package/openclaw',
  },
  amp: {
    keyEnv: 'AMP_API_KEY',
    keyLabel: 'Amp API key',
    runCmd: 'amp -x "{task}"',
    taskPlaceholder: 'e.g. Fix the failing tests in this repo',
    baseUrlEnv: '',
    modelEnv: '',
    mcpFile: '~/.config/amp/settings.json',
    configLabel: 'MCP servers',
    configFormat: 'json',
    configExample: `{
  "amp.mcpServers": {
    "playwright": { "command": "npx", "args": ["-y", "@playwright/mcp@latest"] },
    "linear": { "url": "https://mcp.linear.app/mcp" }
  }
}`,
    installCmd: 'npm i -g @sourcegraph/amp@0.0.1786119357-g68379f',
    cliBin: 'amp',
    runFlags: '',
    docsUrl: 'https://ampcode.com/manual',
  },
}

// Each MCP server is described once, agent-independently; a renderer per agent turns a
// selection into that agent's config file shape.

export type McpKind = 'stdio' | 'http'

export interface McpServer {
  id: string
  name: string
  kind: McpKind
  /** stdio: the npm package run via npx. */
  package?: string
  /** http: the remote endpoint. */
  url?: string
  /** Vault variables this server needs. */
  env?: string[]
  note: string
}

export const MCP_SERVERS: McpServer[] = [
  {
    id: 'github',
    name: 'GitHub',
    kind: 'stdio',
    package: '@modelcontextprotocol/server-github',
    env: ['GITHUB_TOKEN'],
    note: 'Read and write issues, PRs and code.',
  },
  {
    id: 'linear',
    name: 'Linear',
    kind: 'http',
    url: 'https://mcp.linear.app/mcp',
    note: 'Hosted — no token needed here.',
  },
  {
    id: 'playwright',
    name: 'Playwright',
    kind: 'stdio',
    package: '@playwright/mcp@latest',
    note: 'Drive a real browser from the agent.',
  },
  {
    id: 'filesystem',
    name: 'Filesystem',
    kind: 'stdio',
    package: '@modelcontextprotocol/server-filesystem',
    note: 'Read and write files outside the working directory.',
  },
  {
    id: 'postgres',
    name: 'Postgres',
    kind: 'stdio',
    package: '@modelcontextprotocol/server-postgres',
    env: ['DATABASE_URL'],
    note: 'Query a database the agent can reach.',
  },
]

export function serverById(id: string): McpServer | undefined {
  return MCP_SERVERS.find(s => s.id === id)
}

/** envVarsFor returns the vault variables a selection needs, de-duplicated and sorted. */
export function envVarsFor(ids: string[]): string[] {
  const out = new Set<string>()
  for (const id of ids) for (const e of serverById(id)?.env ?? []) out.add(e)
  return [...out].sort()
}

// `${VAR}` is the interpolation every one of these agents understands.
function ref(name: string): string {
  return `\${${name}}`
}

function envObject(s: McpServer): Record<string, string> | undefined {
  if (!s.env?.length) return undefined
  return Object.fromEntries(s.env.map(e => [e, ref(e)]))
}

// Unknown ids are dropped rather than becoming half-written config entries.
function resolve(ids: string[]): McpServer[] {
  return ids.map(serverById).filter((s): s is McpServer => !!s)
}

function renderClaude(servers: McpServer[]): string {
  const mcpServers: Record<string, unknown> = {}
  for (const s of servers) {
    mcpServers[s.id] =
      s.kind === 'http'
        ? { type: 'http', url: s.url }
        : { command: 'npx', args: ['-y', s.package], ...(envObject(s) ? { env: envObject(s) } : {}) }
  }
  return JSON.stringify({ mcpServers }, null, 2)
}

function renderAmp(servers: McpServer[]): string {
  const out: Record<string, unknown> = {}
  for (const s of servers) {
    out[s.id] =
      s.kind === 'http'
        ? { url: s.url }
        : { command: 'npx', args: ['-y', s.package], ...(envObject(s) ? { env: envObject(s) } : {}) }
  }
  return JSON.stringify({ 'amp.mcpServers': out }, null, 2)
}

function renderOpencode(servers: McpServer[]): string {
  const mcp: Record<string, unknown> = {}
  for (const s of servers) {
    mcp[s.id] =
      s.kind === 'http'
        ? { type: 'remote', url: s.url, enabled: true }
        : { type: 'local', command: ['npx', '-y', s.package], enabled: true }
  }
  return JSON.stringify({ $schema: 'https://opencode.ai/config.json', mcp }, null, 2)
}

function renderGrok(servers: McpServer[]): string {
  const mcpServers: Record<string, unknown> = {}
  for (const s of servers) {
    mcpServers[s.id] =
      s.kind === 'http'
        ? { name: s.id, transport: 'http', url: s.url }
        : { name: s.id, transport: 'stdio', command: 'npx', args: ['-y', s.package] }
  }
  return JSON.stringify({ mcpServers }, null, 2)
}

// Hand-written TOML: the shapes are tiny and fixed, so a library would cost more than it saves.
function renderCodex(servers: McpServer[]): string {
  const blocks: string[] = []
  for (const s of servers) {
    const lines = [`[mcp_servers.${s.id}]`]
    if (s.kind === 'http') {
      lines.push(`url = ${JSON.stringify(s.url)}`)
    } else {
      lines.push('command = "npx"')
      lines.push(`args = ["-y", ${JSON.stringify(s.package)}]`)
      const env = envObject(s)
      if (env) {
        lines.push(`env = { ${Object.entries(env).map(([k, v]) => `${k} = ${JSON.stringify(v)}`).join(', ')} }`)
      }
    }
    blocks.push(lines.join('\n'))
  }
  return blocks.join('\n\n')
}

const RENDERERS: Record<string, (s: McpServer[]) => string> = {
  claude: renderClaude,
  amp: renderAmp,
  opencode: renderOpencode,
  grok: renderGrok,
  codex: renderCodex,
}

export function canPick(agentId: string): boolean {
  return agentId in RENDERERS
}

/** renderMcpConfig renders the agent's config file; unknown agents fall back to Claude's shape, so check canPick first. */
export function renderMcpConfig(agentId: string, ids: string[]): string {
  return (RENDERERS[agentId] ?? renderClaude)(resolve(ids))
}

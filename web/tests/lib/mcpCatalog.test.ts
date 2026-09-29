import { describe, it, expect } from 'vitest'
import { MCP_SERVERS, serverById, renderMcpConfig, envVarsFor } from '../../src/lib/mcpCatalog'

describe('catalog', () => {
  it('describes each server once, independently of any agent schema', () => {
    const gh = serverById('github')
    expect(gh?.kind).toBe('stdio')
    expect(gh?.package).toBe('@modelcontextprotocol/server-github')
    expect(gh?.env).toEqual(['GITHUB_TOKEN'])
  })

  it('has a stable, unique id for every entry', () => {
    const ids = MCP_SERVERS.map(s => s.id)
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('collects the vault variables a selection needs', () => {
    expect(envVarsFor(['github', 'linear'])).toEqual(['GITHUB_TOKEN'])
    expect(envVarsFor([])).toEqual([])
  })
})

describe('renderMcpConfig — one selection, five schemas', () => {
  it('writes Claude .mcp.json', () => {
    const out = JSON.parse(renderMcpConfig('claude', ['github', 'linear']))
    expect(out.mcpServers.github).toEqual({
      command: 'npx',
      args: ['-y', '@modelcontextprotocol/server-github'],
      env: { GITHUB_TOKEN: '${GITHUB_TOKEN}' },
    })
    // A remote server is a different shape in the same file.
    expect(out.mcpServers.linear).toEqual({ type: 'http', url: 'https://mcp.linear.app/mcp' })
  })

  it('writes Amp settings.json under its namespaced key', () => {
    const out = JSON.parse(renderMcpConfig('amp', ['linear']))
    expect(out['amp.mcpServers'].linear).toEqual({ url: 'https://mcp.linear.app/mcp' })
  })

  it('writes OpenCode with its local/remote discriminator', () => {
    const out = JSON.parse(renderMcpConfig('opencode', ['github', 'linear']))
    expect(out.mcp.github).toEqual({
      type: 'local',
      command: ['npx', '-y', '@modelcontextprotocol/server-github'],
      enabled: true,
    })
    expect(out.mcp.linear).toEqual({ type: 'remote', url: 'https://mcp.linear.app/mcp', enabled: true })
  })

  it('writes Grok with its transport field', () => {
    const out = JSON.parse(renderMcpConfig('grok', ['github']))
    expect(out.mcpServers.github.transport).toBe('stdio')
    expect(out.mcpServers.github.name).toBe('github')
  })

  it('writes Codex as TOML, not JSON', () => {
    const out = renderMcpConfig('codex', ['github'])
    expect(out).toContain('[mcp_servers.github]')
    expect(out).toContain('command = "npx"')
    expect(out).toContain('args = ["-y", "@modelcontextprotocol/server-github"]')
    expect(out).toContain('env = { GITHUB_TOKEN = "${GITHUB_TOKEN}" }')
    expect(() => JSON.parse(out)).toThrow()
  })

  it('produces valid empty config when nothing is selected', () => {
    expect(JSON.parse(renderMcpConfig('claude', []))).toEqual({ mcpServers: {} })
    expect(renderMcpConfig('codex', []).trim()).toBe('')
  })

  it('ignores an unknown server id rather than emitting a broken entry', () => {
    const out = JSON.parse(renderMcpConfig('claude', ['github', 'nope']))
    expect(Object.keys(out.mcpServers)).toEqual(['github'])
  })

  it('emits parseable output for every JSON agent', () => {
    for (const agent of ['claude', 'amp', 'opencode', 'grok'] as const) {
      expect(() => JSON.parse(renderMcpConfig(agent, ['github', 'linear', 'playwright'])), agent).not.toThrow()
    }
  })
})

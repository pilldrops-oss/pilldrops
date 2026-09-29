import { describe, it, expect } from 'vitest'
import { PROVIDER_SETTINGS, managedVaultNames, launchSecretRefs } from '../../src/lib/providerSettings'
import { AGENT_CONFIG } from '../../src/components/launchConfig'

const byLabel = (l: string) => PROVIDER_SETTINGS.find(p => p.label.startsWith(l))!

describe('provider settings', () => {
  it('offers a key for every provider', () => {
    for (const p of PROVIDER_SETTINGS) expect(p.keyEnv).toMatch(/^[A-Z][A-Z0-9_]*$/)
  })

  it('gives Anthropic a base URL used by both the portal and launches', () => {
    const p = byLabel('Anthropic')
    expect(p.baseUrlEnv).toBe('ANTHROPIC_BASE_URL')
    expect(p.modelEnv).toBe('ANTHROPIC_MODEL')
    expect(p.baseUrlUse).toEqual(['configure', 'launch'])
  })

  it('gives xAI a base URL — its CLI reads GROK_BASE_URL', () => {
    const p = byLabel('xAI')
    expect(p.baseUrlEnv).toBe('GROK_BASE_URL')
    expect(p.baseUrlUse).toEqual(['launch'])
  })

  it('gives OpenAI a base URL for the portal, not the Codex CLI', () => {
    const p = byLabel('OpenAI')
    expect(p.baseUrlEnv).toBe('OPENAI_BASE_URL')
    expect(p.baseUrlUse).toEqual(['configure'])
  })

  it('leaves Amp key-only and explains why', () => {
    const p = byLabel('Amp')
    expect(p.baseUrlEnv).toBeUndefined()
    expect(p.note).toMatch(/settings file/i)
  })

  it('takes launch env names from AGENT_CONFIG so the two cannot drift', () => {
    expect(byLabel('Anthropic').baseUrlEnv).toBe(AGENT_CONFIG.claude.baseUrlEnv)
    expect(byLabel('xAI').baseUrlEnv).toBe(AGENT_CONFIG.grok.baseUrlEnv)
  })

  it('claims every name it renders, so none appear as custom secrets too', () => {
    const managed = managedVaultNames()
    for (const p of PROVIDER_SETTINGS) {
      expect(managed.has(p.keyEnv)).toBe(true)
      if (p.baseUrlEnv) expect(managed.has(p.baseUrlEnv)).toBe(true)
      if (p.modelEnv) expect(managed.has(p.modelEnv)).toBe(true)
    }
  })
})

describe('launchSecretRefs', () => {
  const claude = AGENT_CONFIG.claude
  const grok = AGENT_CONFIG.grok
  const codex = AGENT_CONFIG.codex
  const none = { key: false, baseUrl: false, model: false }

  it('references the stored key when one was not typed', () => {
    expect(launchSecretRefs(claude, ['ANTHROPIC_API_KEY'], none)).toEqual(['ANTHROPIC_API_KEY'])
  })

  it('does not reference the key when the user typed one', () => {
    // A typed key travels as a plain env entry; a vault ref too would set it twice, stored value winning.
    expect(launchSecretRefs(claude, ['ANTHROPIC_API_KEY'], { ...none, key: true })).toEqual([])
  })

  it('carries the stored gateway URL and model into the launch', () => {
    expect(launchSecretRefs(claude,
      ['ANTHROPIC_API_KEY', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_MODEL'], none))
      .toEqual(['ANTHROPIC_API_KEY', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_MODEL'])
  })

  it('lets a typed gateway URL win over the stored one', () => {
    // The server appends the stored value after the typed one, so the override would be lost.
    expect(launchSecretRefs(claude,
      ['ANTHROPIC_API_KEY', 'ANTHROPIC_BASE_URL'], { ...none, baseUrl: true }))
      .toEqual(['ANTHROPIC_API_KEY'])
  })

  it('carries a stored gateway even when the key was typed', () => {
    expect(launchSecretRefs(claude,
      ['ANTHROPIC_API_KEY', 'ANTHROPIC_BASE_URL'], { ...none, key: true }))
      .toEqual(['ANTHROPIC_BASE_URL'])
  })

  it('sends grok its own base URL', () => {
    expect(launchSecretRefs(grok, ['GROK_API_KEY', 'GROK_BASE_URL'], none))
      .toEqual(['GROK_API_KEY', 'GROK_BASE_URL'])
  })

  it('never sends an agent a variable its CLI does not read', () => {
    // Codex keeps its endpoint in config.toml; OPENAI_BASE_URL is for the portal's own calls.
    expect(launchSecretRefs(codex, ['OPENAI_API_KEY', 'OPENAI_BASE_URL', 'OPENAI_MODEL'], none))
      .toEqual(['OPENAI_API_KEY'])
  })

  it('only references what the vault actually holds', () => {
    expect(launchSecretRefs(claude, ['ANTHROPIC_BASE_URL'], none)).toEqual(['ANTHROPIC_BASE_URL'])
    expect(launchSecretRefs(claude, [], none)).toEqual([])
  })

  it('returns nothing when the template is not an agent', () => {
    expect(launchSecretRefs(null, ['ANTHROPIC_API_KEY'], none)).toEqual([])
  })
})

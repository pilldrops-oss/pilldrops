import { describe, it, expect } from 'vitest'
import { quickLaunchable } from '../../src/components/launchConfig'
import type { Template } from '../../src/types'

const agent = (over: Partial<Template> = {}): Template => ({
  id: 'claude', name: 'Claude Code', description: '', tags: ['agent'],
  language: 'node', cpu: 1, memory_mb: 1024, visibility: 'public',
  kind: 'agent', agent: { key_env: 'ANTHROPIC_API_KEY' },
  ...over,
} as Template)

const plain = (over: Partial<Template> = {}): Template => ({
  id: 'base', name: 'Base', description: '', tags: [], language: 'shell',
  cpu: 1, memory_mb: 512, visibility: 'public',
  ...over,
} as Template)

describe('quickLaunchable', () => {
  it('allows an agent whose key is already in the vault', () => {
    expect(quickLaunchable(agent(), ['ANTHROPIC_API_KEY'])).toBe(true)
  })

  it('still excludes an agent with no key stored', () => {
    expect(quickLaunchable(agent(), [])).toBe(false)
  })

  it('excludes an agent when the vault holds a DIFFERENT key', () => {
    expect(quickLaunchable(agent(), ['OPENAI_API_KEY'])).toBe(false)
  })

  it('excludes an agent whose template declares no key env', () => {
    expect(quickLaunchable(agent({ agent: {} }), ['ANTHROPIC_API_KEY'])).toBe(false)
  })

  it('excludes agents when no vault information is supplied', () => {
    expect(quickLaunchable(agent())).toBe(false)
  })

  it('still allows a zero-input template', () => {
    expect(quickLaunchable(plain())).toBe(true)
  })

  it('still blocks a required non-secret env with no default', () => {
    expect(
      quickLaunchable(plain({ env: [{ name: 'TARGET_URL', required: true }] })),
    ).toBe(false)
  })

  it('still blocks a required primary input', () => {
    expect(
      quickLaunchable(plain({ primary_input: { type: 'url', label: 'URL', required: true } })),
    ).toBe(false)
  })
})

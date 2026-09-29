import { describe, expect, it } from 'vitest'
import { agentTabsVisible, tabGroupsFor } from '../../src/pages/agentTabGate'

// The agent tabs are gated on the manifest's kind, never on tags: openclaw is tagged "agent"
// but is kind=app, and every agent route behind the tab would 404 for it.

describe('agentTabsVisible', () => {
  it('shows the agent tabs only for kind=agent', () => {
    expect(agentTabsVisible({ kind: 'agent' })).toBe(true)
    expect(agentTabsVisible({ kind: 'app' })).toBe(false)
    expect(agentTabsVisible({ kind: 'browser' })).toBe(false)
    expect(agentTabsVisible({ kind: 'desktop' })).toBe(false)
  })

  it('does not match a template that is merely tagged "agent"', () => {
    expect(agentTabsVisible({ kind: 'app', tags: ['agent', 'chat'] })).toBe(false)
  })

  it('hides them when the manifest has not loaded, rather than guessing', () => {
    expect(agentTabsVisible(undefined)).toBe(false)
    expect(agentTabsVisible({})).toBe(false)
  })
})

describe('tabGroupsFor', () => {
  it('gives an agent workstation five groups instead of twelve tabs', () => {
    const groups = tabGroupsFor({ kind: 'agent', agent: { id: 'claude' } })
    expect(groups.map(g => g.label)).toEqual(
      ['Agent', 'Timeline', 'Workspace', 'Connect', 'Settings'])
    expect(groups.flatMap(g => g.members).length).toBeGreaterThan(groups.length)
  })

  it('leaves a non-agent sandbox’s strip flat and unchanged', () => {
    const groups = tabGroupsFor({ kind: 'app' })
    for (const g of groups) expect(g.members).toHaveLength(1)
    expect(groups.some(g => g.label === 'Agent')).toBe(false)
    expect(groups.some(g => g.label === 'Timeline')).toBe(false)
    expect(groups.some(g => g.label === 'Connect')).toBe(false)
  })

  it('never puts an agent-only surface in a non-agent strip', () => {
    const ids = tabGroupsFor({ kind: 'app' }).flatMap(g => g.members.map(m => m.id))
    for (const agentOnly of ['agent', 'timeline', 'connect', 'model', 'keys', 'integrations', 'channels']) {
      expect(ids).not.toContain(agentOnly)
    }
  })

  it('omits the model and key tabs for an agent template that declares no agent block', () => {
    const ids = tabGroupsFor({ kind: 'agent' }).flatMap(g => g.members.map(m => m.id))
    expect(ids).toContain('agent')
    expect(ids).not.toContain('model')
    expect(ids).toContain('info')
  })
})

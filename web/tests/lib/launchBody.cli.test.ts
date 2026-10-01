import { describe, it, expect } from 'vitest'
import { buildCliSnippet } from '../../src/lib/launchBody'
import type { Template } from '../../src/types'

const tpl: Template = { id: 'node', name: 'Node + TypeScript', description: '', cpu: 1, memory_mb: 512, visibility: 'public' }

// B20: the snippet must match the form (resource overrides and public), and
// must use flags pilldrops-cli's `sandbox create` actually has — --vcpu and
// --mem, never --memory, and no --name (create has none; only fork/rename do).
describe('buildCliSnippet (B20)', () => {
  it('adds --vcpu and --mem from the form', () => {
    const snippet = buildCliSnippet({ tpl, start: '', port: '', envList: [], cpu: 4, mem: 4096 })
    expect(snippet).toContain('--vcpu 4')
    expect(snippet).toContain('--mem 4096')
    expect(snippet).not.toContain('--memory')
  })

  it('adds --public when the preview is public', () => {
    const snippet = buildCliSnippet({ tpl, start: '', port: '', envList: [], isPublic: true })
    expect(snippet).toContain('--public')
  })

  it('omits --public when the preview is token-gated', () => {
    const snippet = buildCliSnippet({ tpl, start: '', port: '', envList: [], isPublic: false })
    expect(snippet).not.toContain('--public')
  })

  it('never emits --name: sandbox create has no such flag', () => {
    const snippet = buildCliSnippet({ tpl, start: '', port: '', envList: [], cpu: 1, mem: 512, isPublic: true })
    expect(snippet).not.toContain('--name')
  })
})

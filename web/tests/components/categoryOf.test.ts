import { describe, it, expect } from 'vitest'
import { categoryOf } from '../../src/components/launchConfig'
import type { Template } from '../../src/types'

const tpl = (id: string, tags: string[] = [], language = ''): Template => ({
  id, name: id, description: '', tags, language,
  cpu: 1, memory_mb: 512, visibility: 'public',
})

describe('categoryOf', () => {
  it('files data services under Databases', () => {
    expect(categoryOf(tpl('postgres', ['database', 'sql', 'postgres']))).toBe('Databases')
    expect(categoryOf(tpl('redis', ['database', 'cache', 'redis']))).toBe('Databases')
    expect(categoryOf(tpl('mysql', ['database', 'sql', 'mysql']))).toBe('Databases')
    expect(categoryOf(tpl('kafka', ['queue', 'streaming', 'kafka']))).toBe('Databases')
  })

  // "mongodb" contains the substring "go".
  it('does not mistake mongodb for a Go template', () => {
    expect(categoryOf(tpl('mongodb', ['database', 'nosql', 'mongodb']))).toBe('Databases')
  })

  it('matches systems keywords on word boundaries, not substrings', () => {
    expect(categoryOf(tpl('django-app', ['python', 'django']))).toBe('Python')
    expect(categoryOf(tpl('go', ['go']))).toBe('Systems')
    expect(categoryOf(tpl('rust', ['rust']))).toBe('Systems')
  })

  it('still classifies the existing catalog correctly', () => {
    expect(categoryOf(tpl('research', ['research', 'python', 'use-case']))).toBe('Use cases')
    expect(categoryOf(tpl('playwright', ['e2e', 'browser']))).toBe('Browser / E2E')
    expect(categoryOf(tpl('android', ['android', 'emulator']))).toBe('Mobile')
    expect(categoryOf(tpl('nextjs', ['node', 'next']))).toBe('Node / Web')
    // Known misfiling, asserted so it stays visible: the `node` tag outranks the
    // `python` language. The fix is precedence, not a fixture change.
    expect(categoryOf(tpl('code-interpreter', ['python', 'node', 'jupyter', 'data-science'], 'python')))
      .toBe('Node / Web')
  })
})

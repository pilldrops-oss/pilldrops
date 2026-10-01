import { describe, it, expect } from 'vitest'
import { matchesSandboxQuery, showNodeColumn, sortSandboxRows } from '../../src/lib/sandboxListSearch'
import type { SandboxRow } from '../../src/types'

const row = (over: Partial<SandboxRow>): SandboxRow => ({
  id: 'sb1', template: 'node', status: 'running', boot_type: '', boot_ms: 0,
  created_at: '2026-01-01T00:00:00Z', expires_at: '', ...over,
})

describe('showNodeColumn', () => {
  it('is false for a tenant list, which never carries node', () => {
    expect(showNodeColumn([row({}), row({ id: 'sb2' })])).toBe(false)
  })

  it('is true once any row carries node, an operator-only field', () => {
    expect(showNodeColumn([row({}), row({ id: 'sb2', node: 'node-4' })])).toBe(true)
  })

  it('is false for an empty list', () => {
    expect(showNodeColumn([])).toBe(false)
  })
})

describe('matchesSandboxQuery', () => {
  it('matches on name, id or template, case-insensitively', () => {
    const r = row({ id: 'sb_abc', name: 'staging-worker', template: 'python-3.11' })
    expect(matchesSandboxQuery(r, 'STAGING')).toBe(true)
    expect(matchesSandboxQuery(r, 'sb_abc')).toBe(true)
    expect(matchesSandboxQuery(r, 'python')).toBe(true)
    expect(matchesSandboxQuery(r, 'nope')).toBe(false)
  })

  it('an empty query matches everything', () => {
    expect(matchesSandboxQuery(row({}), '  ')).toBe(true)
  })
})

describe('sortSandboxRows', () => {
  const a = row({ id: 'a', name: 'bravo', status: 'running', created_at: '2026-01-01T00:00:00Z' })
  const b = row({ id: 'b', name: 'alpha', status: 'paused', created_at: '2026-01-03T00:00:00Z' })
  const c = row({ id: 'c', name: 'charlie', status: 'running', created_at: '2026-01-02T00:00:00Z' })

  it('recent: running first, then newest', () => {
    expect(sortSandboxRows([a, b, c], 'recent').map(s => s.id)).toEqual(['c', 'a', 'b'])
  })

  it('name: alphabetical by name (or id)', () => {
    expect(sortSandboxRows([a, b, c], 'name').map(s => s.id)).toEqual(['b', 'a', 'c'])
  })

  it('oldest: created_at ascending', () => {
    expect(sortSandboxRows([a, b, c], 'oldest').map(s => s.id)).toEqual(['a', 'c', 'b'])
  })

  it('does not mutate the input array', () => {
    const input = [a, b, c]
    sortSandboxRows(input, 'name')
    expect(input).toEqual([a, b, c])
  })
})

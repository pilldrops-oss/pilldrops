import { describe, it, expect } from 'vitest'
import { scoreTemplate, sortTemplates } from '../../src/lib/templateSearch'
import type { Template } from '../../src/types'

const t = (id: string, tags: string[] = [], name?: string): Template => ({
  id, name: name ?? id, description: '', tags, language: '',
  cpu: 1, memory_mb: 512, visibility: 'public',
})

describe('scoreTemplate', () => {
  it('ranks an exact id above a name prefix above a tag', () => {
    const exact = scoreTemplate(t('redis'), 'redis')
    const prefix = scoreTemplate(t('redis-stack', [], 'Redis Stack'), 'redis')
    const tag = scoreTemplate(t('cache-thing', ['redis']), 'redis')
    expect(exact).toBeGreaterThan(prefix)
    expect(prefix).toBeGreaterThan(tag)
  })

  it('finds a template through a synonym it does not literally contain', () => {
    // "e2e" appears nowhere in the Playwright entry's own text.
    expect(scoreTemplate(t('playwright', ['browser']), 'e2e')).toBeGreaterThan(0)
    expect(scoreTemplate(t('postgres', ['database']), 'db')).toBeGreaterThan(0)
  })

  it('tolerates a one-character typo on a reasonably long query', () => {
    expect(scoreTemplate(t('postgres', ['database']), 'postgre')).toBeGreaterThan(0)
    expect(scoreTemplate(t('postgres', ['database']), 'postgrse')).toBeGreaterThan(0)
  })

  it('does not match an unrelated query', () => {
    expect(scoreTemplate(t('postgres', ['database']), 'kubernetes')).toBe(0)
  })

  it('scores everything when the query is empty', () => {
    expect(scoreTemplate(t('anything'), '')).toBeGreaterThan(0)
  })
})

describe('sortTemplates', () => {
  const list = [t('b-tpl'), t('a-tpl'), t('c-tpl')]

  it('orders by name when asked', () => {
    expect(sortTemplates(list, 'name', '').map(x => x.id)).toEqual(['a-tpl', 'b-tpl', 'c-tpl'])
  })

  it('preserves catalog order for the recommended default', () => {
    expect(sortTemplates(list, 'recommended', '').map(x => x.id)).toEqual(['b-tpl', 'a-tpl', 'c-tpl'])
  })

  it('orders by relevance when a query is present', () => {
    const r = sortTemplates([t('other'), t('redis')], 'recommended', 'redis')
    expect(r[0].id).toBe('redis')
  })
})

import { describe, it, expect } from 'vitest'
import { roleOf, linkEnvFor, isDatastore } from '../../src/lib/capability'
import type { Template } from '../../src/types'

// Roles derive from kind and tags, never from a keyword table, so new templates stay composable.
const tpl = (o: Partial<Template>): Template => ({ id: 'x', name: 'X', description: '', cpu: 1, memory_mb: 512, visibility: 'public', ...o }) as Template

describe('roleOf', () => {
  it('reads a datastore from kind=service plus its tags', () => {
    expect(roleOf(tpl({ id: 'postgres', kind: 'service', tags: ['database', 'sql', 'postgres'] }))).toBe('database')
    expect(roleOf(tpl({ id: 'mysql', kind: 'service', tags: ['database', 'sql', 'mysql'] }))).toBe('database')
  })

  // Redis carries both tags; if database won, "postgres and redis" would fight over DATABASE_URL.
  it('prefers cache over database when a service claims both', () => {
    expect(roleOf(tpl({ id: 'redis', kind: 'service', tags: ['database', 'cache', 'redis'] }))).toBe('cache')
  })

  it('reads app and browser roles from kind', () => {
    expect(roleOf(tpl({ id: 'fastapi', kind: 'app', tags: ['python', 'api'] }))).toBe('app')
    expect(roleOf(tpl({ id: 'playwright', kind: 'test', tags: ['browser', 'e2e'] }))).toBe('browser')
  })

  it('falls back to other for an unclassified template', () => {
    expect(roleOf(tpl({ id: 'mystery' }))).toBe('other')
  })

  it('treats code and shell kinds as apps, since that is what they run', () => {
    expect(roleOf(tpl({ id: 'node-20', kind: 'code' }))).toBe('app')
    expect(roleOf(tpl({ id: 'base', kind: 'shell' }))).toBe('app')
  })
})

describe('isDatastore', () => {
  it('covers both databases and caches', () => {
    expect(isDatastore('database')).toBe(true)
    expect(isDatastore('cache')).toBe(true)
    expect(isDatastore('app')).toBe(false)
    expect(isDatastore('agent')).toBe(false)
  })
})

describe('linkEnvFor', () => {
  it('names the variable a consumer conventionally expects', () => {
    expect(linkEnvFor(tpl({ id: 'postgres', kind: 'service', tags: ['database', 'sql'] }))).toBe('DATABASE_URL')
    expect(linkEnvFor(tpl({ id: 'mysql', kind: 'service', tags: ['database', 'sql'] }))).toBe('DATABASE_URL')
    expect(linkEnvFor(tpl({ id: 'redis', kind: 'service', tags: ['database', 'cache'] }))).toBe('REDIS_URL')
  })

  it('falls back to a name derived from the service, never to nothing', () => {
    expect(linkEnvFor(tpl({ id: 'clickhouse', kind: 'service', tags: ['database'] }))).toBe('CLICKHOUSE_URL')
  })

  it('offers no variable for something that is not a datastore', () => {
    expect(linkEnvFor(tpl({ id: 'fastapi', kind: 'app' }))).toBeNull()
  })
})

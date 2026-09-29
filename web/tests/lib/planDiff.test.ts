import { describe, it, expect } from 'vitest'
import { diffPlans, hasChanges } from '../../src/lib/planDiff'
import type { Plan, PlannedService } from '../../src/lib/plan'

const svc = (id: string, template: string, extra: Partial<PlannedService> = {}): PlannedService =>
  ({
    id,
    role: id === 'api' ? 'app' : 'database',
    template: { id: template, name: template, cpu: 1, memory_mb: 512 } as PlannedService['template'],
    reason: 'because',
    ...extra,
  })

const plan = (services: PlannedService[], links: Plan['links'] = []): Plan =>
  ({ services, links, gaps: [], confidence: 'derived' })

describe('diffPlans', () => {
  it('reports a service that was added', () => {
    const d = diffPlans(plan([svc('api', 'node-20')]), plan([svc('api', 'node-20'), svc('cache', 'redis')]))
    expect(d.added.map(s => s.id)).toEqual(['cache'])
    expect(d.removed).toEqual([])
    expect(d.changed).toEqual([])
  })

  it('reports a service that was removed', () => {
    const d = diffPlans(plan([svc('api', 'node-20'), svc('db', 'postgres')]), plan([svc('api', 'node-20')]))
    expect(d.removed.map(s => s.id)).toEqual(['db'])
  })

  it('reports a service whose template changed, naming both', () => {
    const d = diffPlans(plan([svc('db', 'postgres')]), plan([svc('db', 'mysql')]))
    expect(d.changed).toHaveLength(1)
    expect(d.changed[0]).toMatchObject({ id: 'db', from: 'postgres', to: 'mysql' })
    expect(d.added).toEqual([])
    expect(d.removed).toEqual([])
  })

  it('reports a link that appeared', () => {
    const before = plan([svc('api', 'node-20'), svc('db', 'postgres')])
    const after = plan(
      [svc('api', 'node-20'), svc('db', 'postgres')],
      [{ from: 'api', to: 'db', env: 'DATABASE_URL', reason: 'x' }],
    )
    expect(diffPlans(before, after).linksAdded.map(l => l.env)).toEqual(['DATABASE_URL'])
  })

  it('reports a link that disappeared', () => {
    const before = plan(
      [svc('api', 'node-20'), svc('db', 'postgres')],
      [{ from: 'api', to: 'db', env: 'DATABASE_URL', reason: 'x' }],
    )
    const after = plan([svc('api', 'node-20'), svc('db', 'postgres')])
    expect(diffPlans(before, after).linksRemoved.map(l => l.env)).toEqual(['DATABASE_URL'])
  })

  it('is empty for an unchanged plan', () => {
    const p = plan([svc('api', 'node-20'), svc('db', 'postgres')], [{ from: 'api', to: 'db', env: 'DATABASE_URL', reason: 'x' }])
    const d = diffPlans(p, p)
    expect(hasChanges(d)).toBe(false)
  })

  it('says there are changes when there are', () => {
    expect(hasChanges(diffPlans(plan([]), plan([svc('api', 'node-20')])))).toBe(true)
  })

  // Callers use hasChanges to decide whether to show a diff, and a first run should just run.
  it('treats a missing previous plan as no diff to show', () => {
    const d = diffPlans(null, plan([svc('api', 'node-20')]))
    expect(hasChanges(d)).toBe(false)
    expect(d.added).toEqual([])
  })

  it('survives a malformed stored snapshot without throwing', () => {
    expect(() => diffPlans({ services: null, links: null } as unknown as Plan, plan([]))).not.toThrow()
  })
})

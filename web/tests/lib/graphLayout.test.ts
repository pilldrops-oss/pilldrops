import { describe, it, expect } from 'vitest'
import { layoutPlan } from '../../src/lib/graphLayout'
import { apiWithDb, apiWithDbAndCache, agentOnly, empty } from './planFixtures'

describe('layoutPlan', () => {
  it('places consumers left of what they depend on', () => {
    const g = layoutPlan(apiWithDb)
    const api = g.nodes.find(n => n.id === 'api')!
    const db = g.nodes.find(n => n.id === 'db')!
    expect(api.x).toBeLessThan(db.x)
  })

  it('carries an edge for every link, labelled with its variable', () => {
    const g = layoutPlan(apiWithDb)
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0].label).toBe('DATABASE_URL')
    expect(g.edges[0].from).toBe('api')
    expect(g.edges[0].to).toBe('db')
  })

  it('stacks several stores without overlapping them', () => {
    const g = layoutPlan(apiWithDbAndCache)
    const stores = g.nodes.filter(n => n.id !== 'api')
    expect(stores).toHaveLength(2)
    expect(stores[0].y).not.toBe(stores[1].y)
    // Same column: they are the same depth from the consumer.
    expect(stores[0].x).toBe(stores[1].x)
  })

  it('is deterministic for the same plan', () => {
    const plan = apiWithDbAndCache
    expect(layoutPlan(plan)).toEqual(layoutPlan(plan))
  })

  it('lays out a single service without an edge', () => {
    const g = layoutPlan(agentOnly)
    expect(g.nodes).toHaveLength(1)
    expect(g.edges).toEqual([])
  })

  it('reports a canvas big enough to hold every node', () => {
    const g = layoutPlan(apiWithDbAndCache)
    for (const n of g.nodes) {
      expect(n.x + n.w).toBeLessThanOrEqual(g.width)
      expect(n.y + n.h).toBeLessThanOrEqual(g.height)
    }
  })

  it('handles an empty plan without producing a zero-sized canvas', () => {
    const g = layoutPlan(empty)
    expect(g.nodes).toEqual([])
    expect(g.width).toBeGreaterThan(0)
    expect(g.height).toBeGreaterThan(0)
  })
})

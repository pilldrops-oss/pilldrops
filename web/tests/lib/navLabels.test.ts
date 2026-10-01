import { describe, it, expect } from 'vitest'
import { CUSTOMER_ROUTES, ADMIN_ROUTES, labelFor } from '../../src/lib/navLabels'

describe('breadcrumb labels', () => {
  it('labels Vault as Vault', () => {
    expect(labelFor('vault', false)).toBe('Vault')
  })

  it('labels Domains as Domains', () => {
    expect(labelFor('domains', false)).toBe('Domains')
  })

  it('labels Domains as Domains', () => {
    expect(labelFor('domains', false)).toBe('Domains')
  })

  it('gives every customer route its own label', () => {
    for (const route of CUSTOMER_ROUTES) {
      const label = labelFor(route, false)
      expect(label, `route "${route}" has no label`).toBeTruthy()
    }
  })

  it('gives every admin route its own label', () => {
    for (const route of ADMIN_ROUTES) {
      expect(labelFor(route, true), `admin route "${route}" has no label`).toBeTruthy()
    }
  })

  it('does not reuse one label for unrelated routes', () => {
    // Deliberate aliases: a detail page belongs to its list, and the index redirects to /sandboxes.
    const aliases = new Set(['', 'sandbox', 'fn'])
    const seen = new Map<string, string>()
    for (const route of CUSTOMER_ROUTES) {
      if (aliases.has(route)) continue
      const label = labelFor(route, false)
      expect(seen.has(label), `"${label}" is used by both "${seen.get(label)}" and "${route}"`).toBe(false)
      seen.set(label, route)
    }
  })

  it('falls back rather than rendering an empty breadcrumb for an unknown route', () => {
    // The customer index redirects to Sandboxes, so that is the least-wrong fallback.
    expect(labelFor('not-a-route', false)).toBe('Sandboxes')
    expect(labelFor('not-a-route', true)).toBe('Overview')
  })
})

import { describe, it, expect } from 'vitest'
import { INTEGRATION_RECIPES, KEY_PLACEHOLDER, integrationGrounding } from '../../src/lib/integrationRecipes'

// The verified public surface; every command a snippet invokes must come from here.
const VERIFIED = [
  'Sandbox.create',
  '.run(',
  'sb.kill()',
  'pilldrops sandbox create',
  'pilldrops sandbox list',
  'pilldrops sandbox logs',
  'pilldrops sandbox destroy',
  'pilldrops exec',
  'pilldrops ai',
  'pilldrops login',
  'pilldrops doctor',
  'pilldrops fs write',
  'claude mcp add',
  '/v1/sandboxes',
  // Verified against internal/httpapi/imagebuild.go, not the portal's in-memory store.
  '/v1/images/build',
  'install.sh',
  'pip install pilldrops',
  'npm install @pilldrops/sdk',
]

describe('integration recipes', () => {
  it('every snippet substitutes the key and never ships a fake one', () => {
    for (const r of INTEGRATION_RECIPES) {
      for (const [client, fn] of Object.entries(r.snippets)) {
        const withKey = fn!('sf_test_123')
        expect(withKey, `${r.id}/${client} must use the key`).toContain('sf_test_123')
        expect(withKey, `${r.id}/${client} must name this server, not a hosted one`).not.toContain('pilldrops.com')
        expect(withKey).not.toContain(KEY_PLACEHOLDER)
        expect(withKey).not.toMatch(/sf_live_[a-z0-9]/)
      }
    }
  })

  it('every command line comes from the verified surface', () => {
    for (const r of INTEGRATION_RECIPES) {
      for (const [client, fn] of Object.entries(r.snippets)) {
        const text = fn!('K')
        const commandy = text
          .split('\n')
          .map(l => l.trim())
          .filter(l => l.startsWith('pilldrops ') || l.startsWith('curl -X') || l.includes('Sandbox.create'))
        for (const line of commandy) {
          expect(
            VERIFIED.some(v => line.includes(v)),
            `${r.id}/${client}: unverified command "${line}"`,
          ).toBe(true)
        }
      }
    }
  })

  it('every recipe answers "what after"', () => {
    for (const r of INTEGRATION_RECIPES) {
      expect(r.after.length, r.id).toBeGreaterThan(40)
      expect(Object.keys(r.snippets).length, r.id).toBeGreaterThan(0)
    }
  })

  it('grounding names every integration for the advisor, plus the REST surface', () => {
    // One line per recipe plus the REST cheat-line.
    const g = integrationGrounding()
    expect(g.length).toBe(INTEGRATION_RECIPES.length + 1)
    for (const line of g) expect(line).not.toMatch(/sf_(live|test|portal)_[A-Za-z0-9]/)
  })
})

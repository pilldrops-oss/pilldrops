import { describe, it, expect } from 'vitest'
import {
  parsePilldropsConfig,
  serialisePilldropsConfig,
  PILLDROPS_FILE,
  type PilldropsConfig,
} from '../../src/lib/pilldropsConfig'

const ids = ['playwright', 'base', 'fastapi']

describe('parsePilldropsConfig', () => {
  it('accepts a minimal file', () => {
    const r = parsePilldropsConfig('{"version":1,"template":"base"}', ids)
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.config.template).toBe('base')
  })

  it('accepts a full file', () => {
    const raw = JSON.stringify({
      version: 1, template: 'playwright',
      resources: { vcpu: 2, memory_mb: 2048 },
      setup: ['npm ci'], start: 'npx playwright test', port: 9323,
      env: ['STRIPE_SECRET_KEY'],
    })
    const r = parsePilldropsConfig(raw, ids)
    expect(r.ok).toBe(true)
    if (r.ok) {
      expect(r.config.resources).toEqual({ vcpu: 2, memory_mb: 2048 })
      expect(r.config.setup).toEqual(['npm ci'])
      expect(r.config.env).toEqual(['STRIPE_SECRET_KEY'])
    }
  })

  it('reports malformed JSON and names the file', () => {
    const r = parsePilldropsConfig('{not json', ids)
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error).toContain(PILLDROPS_FILE)
  })

  it('reports an unknown template and names the id', () => {
    const r = parsePilldropsConfig('{"version":1,"template":"kubernetes"}', ids)
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error).toContain('kubernetes')
  })

  it('reports an unrecognised version', () => {
    const r = parsePilldropsConfig('{"version":99,"template":"base"}', ids)
    expect(r.ok).toBe(false)
    if (!r.ok) expect(r.error).toContain('99')
  })

  // The file is committed to a repository, so accepting env values would leak credentials.
  it('rejects env given as values, and explains why', () => {
    const r = parsePilldropsConfig(
      '{"version":1,"template":"base","env":{"STRIPE_KEY":"sk_live_x"}}', ids)
    expect(r.ok).toBe(false)
    if (!r.ok) {
      expect(r.error).toMatch(/name/i)
      expect(r.error).not.toContain('sk_live_x') // never echo a secret back
    }
  })

  it('accepts env as an array of names', () => {
    const r = parsePilldropsConfig('{"version":1,"template":"base","env":["A","B"]}', ids)
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.config.env).toEqual(['A', 'B'])
  })

  it('allows a file with no template at all', () => {
    const r = parsePilldropsConfig('{"version":1,"start":"npm start"}', ids)
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.config.start).toBe('npm start')
  })
})

describe('serialisePilldropsConfig', () => {
  const cfg: PilldropsConfig = {
    version: 1, template: 'playwright',
    resources: { vcpu: 2, memory_mb: 2048 },
    setup: ['npm ci'], start: 'npx playwright test', port: 9323,
    env: ['STRIPE_SECRET_KEY'],
  }

  it('round-trips through parse', () => {
    const r = parsePilldropsConfig(serialisePilldropsConfig(cfg), ids)
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.config).toEqual(cfg)
  })

  it('omits empty fields rather than writing nulls', () => {
    const out = serialisePilldropsConfig({ version: 1, template: 'base' })
    expect(out).not.toContain('null')
    expect(out).not.toContain('"setup"')
  })

  it('is human-editable — indented, newline-terminated', () => {
    const out = serialisePilldropsConfig(cfg)
    expect(out).toContain('\n  ')
    expect(out.endsWith('\n')).toBe(true)
  })
})

import { describe, it, expect } from 'vitest'
import { buildPilldropsExport } from '../../src/components/launchConfig'
import { parsePilldropsConfig, serialisePilldropsConfig } from '../../src/lib/pilldropsConfig'
import type { Template } from '../../src/types'

const ids = ['playwright', 'base']

const tpl = (over: Partial<Template> = {}): Template => ({
  id: 'playwright',
  name: 'Playwright',
  description: '',
  tags: [],
  language: 'node',
  cpu: 1,
  memory_mb: 512,
  visibility: 'public',
  env: [{ name: 'STRIPE_SECRET_KEY', required: true, secret: true, value: 'sk_live_shouldnotleak' }],
  ...over,
} as Template)

describe('buildPilldropsExport', () => {
  it('builds a config from current form state', () => {
    const cfg = buildPilldropsExport({
      tpl: tpl(),
      cpu: 2,
      mem: 2048,
      storage: 1024,
      start: 'npx playwright test',
      setupCmds: ['npm ci'],
      port: '9323',
      previewPort: 9323,
      customEnv: [{ name: 'CUSTOM_TOKEN', value: 'super-secret-value' }],
    })
    expect(cfg.version).toBe(1)
    expect(cfg.template).toBe('playwright')
    expect(cfg.resources).toEqual({ vcpu: 2, memory_mb: 2048, storage_mb: 1024 })
    expect(cfg.setup).toEqual(['npm ci'])
    expect(cfg.start).toBe('npx playwright test')
    expect(cfg.port).toBe(9323)
    expect(cfg.env).toEqual(['STRIPE_SECRET_KEY', 'CUSTOM_TOKEN'])
  })

  it('round-trips through the parser', () => {
    const cfg = buildPilldropsExport({
      tpl: tpl(),
      cpu: 2,
      mem: 2048,
      storage: 1024,
      start: 'npx playwright test',
      setupCmds: ['npm ci'],
      port: '9323',
      previewPort: 9323,
      customEnv: [{ name: 'CUSTOM_TOKEN', value: 'super-secret-value' }],
    })
    const serialised = serialisePilldropsConfig(cfg)
    const r = parsePilldropsConfig(serialised, ids)
    expect(r.ok).toBe(true)
    if (r.ok) expect(r.config).toEqual(cfg)
  })

  // The export is committed to a repository, so env values must never appear in it.
  it('never includes an env value, only names', () => {
    const cfg = buildPilldropsExport({
      tpl: tpl(),
      cpu: 1,
      mem: 512,
      storage: 1024,
      start: '',
      setupCmds: [],
      port: '',
      previewPort: 8000,
      customEnv: [{ name: 'CUSTOM_TOKEN', value: 'super-secret-value' }],
    })
    const serialised = serialisePilldropsConfig(cfg)
    expect(serialised).not.toContain('sk_live_shouldnotleak')
    expect(serialised).not.toContain('super-secret-value')
    expect(serialised).toContain('STRIPE_SECRET_KEY')
    expect(serialised).toContain('CUSTOM_TOKEN')
  })

  it('omits an unselected template rather than writing null', () => {
    const cfg = buildPilldropsExport({
      tpl: null,
      cpu: 1,
      mem: 512,
      storage: 1024,
      start: 'go run .',
      setupCmds: [],
      port: '',
      previewPort: 8000,
      customEnv: [],
    })
    expect(cfg.template).toBeUndefined()
    expect(cfg.start).toBe('go run .')
    expect(cfg.env).toBeUndefined()
  })
})

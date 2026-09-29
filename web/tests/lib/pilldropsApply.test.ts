import { describe, it, expect } from 'vitest'
import { mergeConfigOverDetection } from '../../src/lib/pilldropsConfig'

const detection = { templateId: 'base', label: 'Node project', start: 'npm start', port: 3000 }

describe('mergeConfigOverDetection', () => {
  it('lets the config win where both speak', () => {
    const m = mergeConfigOverDetection(
      { version: 1, template: 'playwright', start: 'npx playwright test' }, detection)
    expect(m.templateId).toBe('playwright')
    expect(m.start).toBe('npx playwright test')
  })

  it('falls back to detection where the config is silent', () => {
    const m = mergeConfigOverDetection({ version: 1, template: 'playwright' }, detection)
    expect(m.port).toBe(3000)
  })

  it('keeps detection entirely when there is no config', () => {
    expect(mergeConfigOverDetection(null, detection)).toEqual(
      expect.objectContaining({ templateId: 'base', start: 'npm start' }))
  })

  it('reports where each value came from', () => {
    const m = mergeConfigOverDetection(
      { version: 1, template: 'playwright' }, detection)
    expect(m.source.templateId).toBe('pilldrops.json')
    expect(m.source.start).toBe('detection')
  })

  it('handles a config with no detection at all', () => {
    const m = mergeConfigOverDetection({ version: 1, start: 'go run .' }, null)
    expect(m.start).toBe('go run .')
    expect(m.source.start).toBe('pilldrops.json')
  })
})

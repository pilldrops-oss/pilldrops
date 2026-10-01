import { describe, it, expect } from 'vitest'
import { previewPortOf } from '../../src/lib/previewPort'

// GET /v1/sandboxes/{id} carries no public_port, only the url built on it, so
// the detail page must read the port from there before falling back.
describe('previewPortOf', () => {
  it('prefers a declared public_port', () => {
    expect(previewPortOf({ public_port: 3000, url: 'http://h/preview/sb1/8000/' })).toBe(3000)
  })

  it('reads the port from a path-form url', () => {
    expect(previewPortOf({ url: 'http://h:9090/preview/sb1/6080/' })).toBe(6080)
  })

  it('reads the port from a host-form url', () => {
    expect(previewPortOf({ url: 'https://6080-sb1.pilldrops.com/?ptok=x' })).toBe(6080)
  })

  it('falls back to desktop_url, then the template, then 8000', () => {
    expect(previewPortOf({ desktop_url: 'http://h/preview/sb1/6080/' })).toBe(6080)
    expect(previewPortOf({}, { default_port: 6080 })).toBe(6080)
    expect(previewPortOf({}, { port: 5000 })).toBe(5000)
    expect(previewPortOf({})).toBe(8000)
  })
})

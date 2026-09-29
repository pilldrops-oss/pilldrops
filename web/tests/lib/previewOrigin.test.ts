import { describe, it, expect, afterEach } from 'vitest'
import { isPreviewCrossOrigin, previewUrl } from '../../src/api'

const original = window.__SANDBOXD__

function setDomain(sandboxDomain: string | undefined) {
  window.__SANDBOXD__ = { key: '', sandboxDomain } as typeof window.__SANDBOXD__
}

afterEach(() => {
  window.__SANDBOXD__ = original
})

describe('isPreviewCrossOrigin', () => {
  it('is true when a preview domain is configured', () => {
    setDomain('pilldrops.com')
    expect(isPreviewCrossOrigin()).toBe(true)
  })

  it('is false when no domain is configured', () => {
    setDomain(undefined)
    expect(isPreviewCrossOrigin()).toBe(false)
  })

  // index.html ships the __SANDBOXD_DOMAIN__ placeholder and the server substitutes it at boot.
  it('is false for the unsubstituted build placeholder', () => {
    setDomain('__SANDBOXD_DOMAIN__')
    expect(isPreviewCrossOrigin()).toBe(false)
  })

  it('is false for an empty string', () => {
    setDomain('')
    expect(isPreviewCrossOrigin()).toBe(false)
  })
})

describe('previewUrl still behaves as before', () => {
  it('uses the subdomain form when a domain is configured', () => {
    setDomain('pilldrops.com')
    expect(previewUrl('sb1', 8000)).toBe('https://8000-sb1.pilldrops.com/')
  })

  it('uses the same-origin path form otherwise', () => {
    setDomain(undefined)
    expect(previewUrl('sb1', 8000)).toBe('/preview/sb1/8000/')
  })

  it('agrees with isPreviewCrossOrigin in both modes', () => {
    setDomain('pilldrops.com')
    expect(previewUrl('sb1', 1).startsWith('https://')).toBe(isPreviewCrossOrigin())
    setDomain(undefined)
    expect(previewUrl('sb1', 1).startsWith('https://')).toBe(isPreviewCrossOrigin())
  })
})

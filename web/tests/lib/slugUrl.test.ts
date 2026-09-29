import { describe, it, expect, afterEach } from 'vitest'
import { slugUrl } from '../../src/api'

const original = window.__SANDBOXD__

function setDomain(sandboxDomain: string | undefined) {
  window.__SANDBOXD__ = { key: '', sandboxDomain } as typeof window.__SANDBOXD__
}

afterEach(() => {
  window.__SANDBOXD__ = original
})

describe('slugUrl', () => {
  it('is https://<slug>.<domain>/ when a preview domain is configured', () => {
    setDomain('pilldrops.com')
    expect(slugUrl('myapp')).toBe('https://myapp.pilldrops.com/')
  })

  it('is undefined without a slug', () => {
    setDomain('pilldrops.com')
    expect(slugUrl(undefined)).toBeUndefined()
    expect(slugUrl('')).toBeUndefined()
  })

  // The path form /preview/<id>/<port>/ has no place for a name.
  it('is undefined without a preview domain', () => {
    setDomain(undefined)
    expect(slugUrl('myapp')).toBeUndefined()
    setDomain('__SANDBOXD_DOMAIN__')
    expect(slugUrl('myapp')).toBeUndefined()
  })
})

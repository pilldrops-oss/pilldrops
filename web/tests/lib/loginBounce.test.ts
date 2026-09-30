import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { listSandboxes } from '../../src/api'

// A server whose login page sends the browser back to /dashboard (make dev has
// no sign-in) turned every 401 into an endless redirect. The dashboard now
// redirects once, then shows the error.
const realLocation = window.location
const realSandboxd = window.__SANDBOXD__
let status = 401
const fetchMock = vi.fn(async () =>
  new Response(status === 200 ? '{"sandboxes":[]}' : '{"error":"invalid api key"}', { status }))

beforeEach(() => {
  status = 401
  fetchMock.mockClear()
  sessionStorage.clear()
  vi.stubGlobal('fetch', fetchMock)
  Object.defineProperty(window, 'location', { configurable: true, value: { href: '/dashboard', pathname: '/dashboard/' } })
  window.__SANDBOXD__ = { key: '' } as typeof window.__SANDBOXD__
})
afterEach(() => {
  vi.unstubAllGlobals()
  Object.defineProperty(window, 'location', { configurable: true, value: realLocation })
  window.__SANDBOXD__ = realSandboxd
})

describe('401 handling', () => {
  it('redirects to login once, then reports the error instead of looping', async () => {
    await expect(listSandboxes()).rejects.toThrow('redirecting to login')
    expect(window.location.href).toBe('/dashboard/login')

    window.location.href = '/dashboard'
    await expect(listSandboxes()).rejects.toMatchObject({ status: 401 })
    expect(window.location.href).toBe('/dashboard')
  })

  it('a successful call re-arms the redirect', async () => {
    await expect(listSandboxes()).rejects.toThrow('redirecting to login')
    status = 200
    await listSandboxes()
    status = 401
    await expect(listSandboxes()).rejects.toThrow('redirecting to login')
  })

  it('sends no Authorization header when there is no key', async () => {
    status = 200
    await listSandboxes()
    const init = (fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1]
    expect(init.headers).not.toHaveProperty('Authorization')
  })
})

import { describe, it, expect, vi, afterEach } from 'vitest'
import { fsDownload } from '../../src/api'

afterEach(() => vi.unstubAllGlobals())

// U5/B15: the filesystem routes now answer with a specific status and a
// message naming what's wrong (404 missing, 403 permission, …). fsDownload
// must carry that body, not just the bare method/path/status.
describe('fsDownload', () => {
  it('carries the server error body, not just the status', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('path not found: /etc/shadow', { status: 404 })))
    await expect(fsDownload('sb1', '/etc/shadow')).rejects.toThrow(/path not found: \/etc\/shadow/)
  })

  it('still returns the blob on success', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('hello', { status: 200 })))
    const blob = await fsDownload('sb1', 'a.txt')
    expect(await blob.text()).toBe('hello')
  })
})

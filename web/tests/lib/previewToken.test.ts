import { describe, expect, it, vi, beforeEach } from 'vitest'
import * as api from '../../src/api'
import { previewToken, previewLink, forgetPreviewTokens, _resetPreviewTokenCache } from '../../src/lib/previewToken'

// Caching is the part worth testing: too lenient and every port toggle is a round trip,
// too strict and one port's token is served for another.
beforeEach(() => {
  _resetPreviewTokenCache()
  vi.restoreAllMocks()
})

describe('previewToken', () => {
  it('asks for a token per port, and never reuses one port’s for another', async () => {
    const get = vi
      .spyOn(api, 'getPreviewToken')
      .mockImplementation(async (_id, port) => `tok-${port}`)

    expect(await previewToken('sb1', 3000)).toBe('tok-3000')
    expect(await previewToken('sb1', 8080)).toBe('tok-8080')

    expect(get).toHaveBeenCalledTimes(2)
    expect(get).toHaveBeenCalledWith('sb1', 3000)
    expect(get).toHaveBeenCalledWith('sb1', 8080)
  })

  it('caches per (sandbox, port) so a port switcher does not re-request', async () => {
    const get = vi.spyOn(api, 'getPreviewToken').mockResolvedValue('tok')

    await previewToken('sb1', 3000)
    await previewToken('sb1', 3000)
    await previewToken('sb1', 3000)

    expect(get).toHaveBeenCalledTimes(1)
  })

  it('shares one request between concurrent callers', async () => {
    // The cache holds promises, not values, so callers in the same tick share one request.
    const get = vi.spyOn(api, 'getPreviewToken').mockImplementation(
      () => new Promise(resolve => setTimeout(() => resolve('tok'), 10)),
    )

    const all = await Promise.all([
      previewToken('sb1', 3000),
      previewToken('sb1', 3000),
      previewToken('sb1', 3000),
    ])

    expect(all).toEqual(['tok', 'tok', 'tok'])
    expect(get).toHaveBeenCalledTimes(1)
  })

  it('does not cache a failure forever', async () => {
    // A token is unavailable while a sandbox is still being placed, then becomes available.
    const get = vi
      .spyOn(api, 'getPreviewToken')
      .mockRejectedValueOnce(new Error('not yet'))
      .mockResolvedValue('tok')

    expect(await previewToken('sb1', 3000)).toBeNull()
    expect(await previewToken('sb1', 3000)).toBe('tok')
    expect(get).toHaveBeenCalledTimes(2)
  })

  it('resolves null rather than rejecting', async () => {
    // A public sandbox needs no token, so a missing one is a bare URL, not a render error.
    vi.spyOn(api, 'getPreviewToken').mockRejectedValue(new Error('403'))
    await expect(previewToken('sb1', 3000)).resolves.toBeNull()
  })

  it('forgetPreviewTokens drops only the named sandbox', async () => {
    vi.spyOn(api, 'getPreviewToken').mockImplementation(async id => `tok-${id}`)
    await previewToken('sb1', 3000)
    await previewToken('sb2', 3000)

    forgetPreviewTokens('sb1')

    const get = vi.spyOn(api, 'getPreviewToken').mockResolvedValue('fresh')
    expect(await previewToken('sb1', 3000)).toBe('fresh') // refetched
    expect(await previewToken('sb2', 3000)).toBe('tok-sb2') // still cached
    expect(get).toHaveBeenCalledTimes(1)
  })
})

describe('previewLink', () => {
  it('attaches the token for the port being linked', async () => {
    vi.spyOn(api, 'getPreviewToken').mockImplementation(async (_id, port) => `tok-${port}`)
    vi.spyOn(api, 'previewUrl').mockImplementation((id, port) => `/preview/${id}/${port}/`)

    expect(await previewLink('sb1', 3000)).toBe('/preview/sb1/3000/?token=tok-3000')
    expect(await previewLink('sb1', 8080)).toBe('/preview/sb1/8080/?token=tok-8080')
  })

  it('returns the bare URL when no token can be had', async () => {
    vi.spyOn(api, 'getPreviewToken').mockRejectedValue(new Error('public sandbox'))
    vi.spyOn(api, 'previewUrl').mockReturnValue('/preview/sb1/3000/')

    expect(await previewLink('sb1', 3000)).toBe('/preview/sb1/3000/')
  })

  it('uses & when the URL already carries a query', async () => {
    vi.spyOn(api, 'getPreviewToken').mockResolvedValue('tok')
    vi.spyOn(api, 'previewUrl').mockReturnValue('/preview/sb1/3000/?a=b')

    expect(await previewLink('sb1', 3000)).toBe('/preview/sb1/3000/?a=b&token=tok')
  })

  it('encodes the token', async () => {
    vi.spyOn(api, 'getPreviewToken').mockResolvedValue('a+b/c=')
    vi.spyOn(api, 'previewUrl').mockReturnValue('/preview/sb1/3000/')

    expect(await previewLink('sb1', 3000)).toBe('/preview/sb1/3000/?token=a%2Bb%2Fc%3D')
  })
})

import { describe, it, expect } from 'vitest'
import { validateGatewayUrl } from '../../src/lib/gatewayUrl'

describe('validateGatewayUrl', () => {
  it('accepts a plain https endpoint unchanged', () => {
    const r = validateGatewayUrl('https://gateway.example.com')
    expect(r).toMatchObject({ ok: true, url: 'https://gateway.example.com' })
    expect((r as { note?: string }).note).toBeUndefined()
  })

  it('strips a trailing /v1 and says so', () => {
    // The client appends /v1 itself, so keeping it would produce /v1/v1/messages.
    const r = validateGatewayUrl('https://litellm.internal/v1')
    expect(r).toMatchObject({ ok: true, url: 'https://litellm.internal' })
    expect((r as { note?: string }).note).toMatch(/\/v1/)
  })

  it('strips trailing slashes', () => {
    expect(validateGatewayUrl('https://gw.example.com/')).toMatchObject({ url: 'https://gw.example.com' })
    expect(validateGatewayUrl('https://gw.example.com/v1/')).toMatchObject({ url: 'https://gw.example.com' })
  })

  it('keeps a meaningful path that is not /v1', () => {
    expect(validateGatewayUrl('https://gw.example.com/anthropic')).toMatchObject({
      ok: true,
      url: 'https://gw.example.com/anthropic',
    })
  })

  it('warns but accepts plain http', () => {
    const r = validateGatewayUrl('http://10.0.0.5:4000')
    expect(r.ok).toBe(true)
    expect((r as { note?: string }).note).toMatch(/clear text/i)
  })

  it('rejects a missing scheme', () => {
    const r = validateGatewayUrl('gateway.example.com')
    expect(r.ok).toBe(false)
    expect((r as { error: string }).error).toMatch(/scheme/i)
  })

  it('rejects a non-http scheme', () => {
    const r = validateGatewayUrl('ftp://gateway.example.com')
    expect(r.ok).toBe(false)
    expect((r as { error: string }).error).toMatch(/http/i)
  })

  it('rejects whitespace inside the URL', () => {
    const r = validateGatewayUrl('https://gw.example.com/my path')
    expect(r.ok).toBe(false)
    expect((r as { error: string }).error).toMatch(/spaces/i)
  })

  it('rejects an empty value', () => {
    expect(validateGatewayUrl('').ok).toBe(false)
    expect(validateGatewayUrl('   ').ok).toBe(false)
  })

  it('tolerates surrounding whitespace from a paste', () => {
    expect(validateGatewayUrl('  https://gw.example.com/v1  ')).toMatchObject({
      ok: true,
      url: 'https://gw.example.com',
    })
  })
})

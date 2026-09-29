import { describe, it, expect, vi } from 'vitest'
import { cancellableCreate } from '../../src/lib/cancellableCreate'
import type { CreateSandboxResp } from '../../src/types'

const resp = (id: string) => ({ id }) as CreateSandboxResp
const never = () => new Promise<CreateSandboxResp>(() => {})

describe('cancellableCreate', () => {
  it('returns the sandbox when it is not cancelled', async () => {
    const destroy = vi.fn()
    const r = await cancellableCreate({ create: () => Promise.resolve(resp('sb1')), destroy })
      .result
    expect(r).toEqual({ cancelled: false, resp: resp('sb1') })
    expect(destroy).not.toHaveBeenCalled()
  })

  // The create is one server call that cancel cannot abort, so a late result is destroyed
  // rather than left running and billing.
  it('destroys the sandbox when the create lands after cancellation', async () => {
    const destroy = vi.fn().mockResolvedValue(undefined)
    let settle: (r: CreateSandboxResp) => void = () => {}
    const create = () => new Promise<CreateSandboxResp>(res => { settle = res })

    const h = cancellableCreate({ create, destroy })
    h.cancel()
    expect(await h.result).toEqual({ cancelled: true })

    settle(resp('sb_late'))
    await new Promise(r => setTimeout(r, 0))
    expect(destroy).toHaveBeenCalledWith('sb_late')
  })

  it('resolves promptly on cancel rather than waiting for the server', async () => {
    const h = cancellableCreate({ create: never, destroy: vi.fn() })
    h.cancel()
    expect(await h.result).toEqual({ cancelled: true })
  })

  it('swallows a failing cleanup', async () => {
    const destroy = vi.fn().mockRejectedValue(new Error('gone'))
    let settle: (r: CreateSandboxResp) => void = () => {}
    const h = cancellableCreate({
      create: () => new Promise<CreateSandboxResp>(res => { settle = res }),
      destroy,
    })
    h.cancel()
    await h.result
    settle(resp('sb2'))
    await expect(new Promise(r => setTimeout(r, 0))).resolves.toBeUndefined()
    expect(destroy).toHaveBeenCalled()
  })

  it('propagates a create failure when not cancelled', async () => {
    const h = cancellableCreate({
      create: () => Promise.reject(new Error('boom')),
      destroy: vi.fn(),
    })
    await expect(h.result).rejects.toThrow('boom')
  })

  it('stays quiet when a cancelled create then fails', async () => {
    const destroy = vi.fn()
    let fail: (e: Error) => void = () => {}
    const h = cancellableCreate({
      create: () => new Promise<CreateSandboxResp>((_, rej) => { fail = rej }),
      destroy,
    })
    h.cancel()
    expect(await h.result).toEqual({ cancelled: true })
    fail(new Error('boom'))
    await new Promise(r => setTimeout(r, 0))
    expect(destroy).not.toHaveBeenCalled()
  })
})

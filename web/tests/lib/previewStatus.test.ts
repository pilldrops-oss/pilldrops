import { describe, it, expect, vi } from 'vitest'
import { pollPreviewReady } from '../../src/lib/previewStatus'
import type { PreviewStatus } from '../../src/types'

const st = (state: PreviewStatus['state']): PreviewStatus =>
  ({ state, sandbox: 'sb1', port: 8000 })

const noSleep = () => Promise.resolve()

describe('pollPreviewReady', () => {
  it('resolves ready on the first successful probe', async () => {
    const probe = vi.fn().mockResolvedValue(st('ready'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('ready')
    expect(probe).toHaveBeenCalledTimes(1)
  })

  it('keeps polling while starting, then resolves ready', async () => {
    const probe = vi.fn()
      .mockResolvedValueOnce(st('starting'))
      .mockResolvedValueOnce(st('starting'))
      .mockResolvedValueOnce(st('ready'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('ready')
    expect(probe).toHaveBeenCalledTimes(3)
  })

  it('gives up at the deadline and reports unreachable', async () => {
    const probe = vi.fn().mockResolvedValue(st('starting'))
    let t = 0
    const r = await pollPreviewReady({
      probe, sleep: noSleep, deadlineMs: 5_000, now: () => (t += 2_000),
    })
    expect(r.state).toBe('unreachable')
    expect(r.lastState).toBe('starting')
  })

  it('stops immediately on app_error — waiting would be a lie', async () => {
    const probe = vi.fn().mockResolvedValue(st('app_error'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('app_error')
    expect(probe).toHaveBeenCalledTimes(1)
  })

  it('stops on no_page — the app is up, it just has no index route', async () => {
    const probe = vi.fn().mockResolvedValue(st('no_page'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('no_page')
  })

  // Single-node deployments cannot probe, so unknown is final rather than retried.
  it('stops immediately on unknown', async () => {
    const probe = vi.fn().mockResolvedValue(st('unknown'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('unknown')
    expect(probe).toHaveBeenCalledTimes(1)
  })

  it('reports unknown, not unreachable, when every probe failed', async () => {
    const probe = vi.fn().mockRejectedValue(new Error('session expired'))
    let t = 0
    const r = await pollPreviewReady({
      probe, sleep: noSleep, deadlineMs: 5_000, now: () => (t += 2_000),
    })
    expect(r.state).toBe('unknown')
  })

  it('still reports unreachable when probes answered but never became ready', async () => {
    const probe = vi.fn()
      .mockResolvedValueOnce(st('starting'))
      .mockRejectedValue(new Error('flaky'))
    let t = 0
    const r = await pollPreviewReady({
      probe, sleep: noSleep, deadlineMs: 5_000, now: () => (t += 2_000),
    })
    expect(r.state).toBe('unreachable')
  })

  it('treats a thrown probe as still starting and retries', async () => {
    const probe = vi.fn()
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValueOnce(st('ready'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('ready')
    expect(probe).toHaveBeenCalledTimes(2)
  })

  it('stops on terminal — no app is registered, waiting is pointless', async () => {
    const probe = vi.fn().mockResolvedValue(st('terminal'))
    const r = await pollPreviewReady({ probe, sleep: noSleep, deadlineMs: 60_000, now: () => 0 })
    expect(r.state).toBe('terminal')
    expect(probe).toHaveBeenCalledTimes(1)
  })
})

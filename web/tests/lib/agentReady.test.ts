import { describe, expect, it, vi } from 'vitest'
import { pollAgentReady } from '../../src/lib/agentReady'

const clock = () => {
  let t = 0
  return { now: () => t, sleep: async (ms: number) => { t += ms } }
}

describe('pollAgentReady', () => {
  it('is ready as soon as the supervisor reports idle', async () => {
    const c = clock()
    const r = await pollAgentReady({
      probe: async () => ({ session: { state: 'idle' } }),
      sleep: c.sleep, now: c.now, deadlineMs: 90_000,
    })
    expect(r.state).toBe('ready')
  })

  it('waits through the boot, then succeeds', async () => {
    const c = clock()
    const states = ['starting', 'starting', 'idle']
    let i = 0
    const probe = vi.fn(async () => {
      const s = states[Math.min(i++, states.length - 1)]
      return { session: { state: s } }
    })
    const r = await pollAgentReady({ probe, sleep: c.sleep, now: c.now, deadlineMs: 90_000 })
    expect(r.state).toBe('ready')
    expect(probe.mock.calls.length).toBeGreaterThan(2)
  })

  it('keeps waiting while the supervisor is not answering at all', async () => {
    const c = clock()
    let i = 0
    const r = await pollAgentReady({
      probe: async () => {
        if (i++ < 3) throw new Error('connection refused')
        return { session: { state: 'idle' } }
      },
      sleep: c.sleep, now: c.now, deadlineMs: 90_000,
    })
    expect(r.state).toBe('ready')
  })

  it('does NOT call a degraded agent ready', async () => {
    // The supervisor reports degraded when the agent process died or the CLI is missing.
    const c = clock()
    const r = await pollAgentReady({
      probe: async () => ({ session: { state: 'degraded' } }),
      sleep: c.sleep, now: c.now, deadlineMs: 90_000,
    })
    expect(r.state).toBe('app_error')
  })

  it('reports a bad answer immediately rather than waiting out the deadline', async () => {
    const c = clock()
    await pollAgentReady({
      probe: async () => ({ session: { state: 'stopped' } }),
      sleep: c.sleep, now: c.now, deadlineMs: 90_000,
    })
    expect(c.now()).toBeLessThan(5_000)
  })

  it('gives up after the deadline instead of hanging forever', async () => {
    const c = clock()
    const r = await pollAgentReady({
      probe: async () => ({ session: { state: 'starting' } }),
      sleep: c.sleep, now: c.now, deadlineMs: 10_000,
    })
    expect(r.state).toBe('unreachable')
  })

  it('treats a turn already running as ready', async () => {
    const c = clock()
    for (const state of ['running', 'awaiting_approval']) {
      const r = await pollAgentReady({
        probe: async () => ({ session: { state } }),
        sleep: c.sleep, now: c.now, deadlineMs: 90_000,
      })
      expect(r.state, state).toBe('ready')
    }
  })
})

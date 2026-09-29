import { describe, expect, it } from 'vitest'
import { backoffMs } from '../../src/hooks/useAgentStream'

// EventSource reconnects at a fixed interval, so a node restart makes every dashboard retry
// in lockstep. These tests pin the herd-breaking, not the exact numbers.

describe('backoffMs', () => {
  it('grows with consecutive failures and stops growing at the ceiling', async () => {
    const worst = (n: number) => backoffMs(n, () => 1)
    expect(worst(0)).toBeLessThan(worst(2))
    expect(worst(2)).toBeLessThan(worst(5))
    expect(worst(50)).toBeLessThanOrEqual(30_000)
    expect(worst(50)).toEqual(worst(20))
  })

  it('never returns a zero delay', () => {
    for (let n = 0; n < 12; n++) {
      expect(backoffMs(n, () => 0)).toBeGreaterThanOrEqual(1_000)
    }
  })

  it('spreads a synchronised herd instead of rescheduling it', () => {
    const delays = Array.from({ length: 200 }, () => backoffMs(4))
    const unique = new Set(delays)
    expect(unique.size).toBeGreaterThan(150)

    // Full jitter spreads delays across the whole window; ±10% noise would cluster them.
    const lo = delays.filter((d) => d < 6_000).length
    const hi = delays.filter((d) => d > 11_000).length
    expect(lo).toBeGreaterThan(20)
    expect(hi).toBeGreaterThan(20)
  })
})

// While a stream is live the journal already carries every turn transition, so the poll
// beside it is only a reconciliation safety net.
describe('pollIntervalMs', () => {
  it('polls faster when the stream is gone than when it is live', async () => {
    const { pollIntervalMs } = await import('../../src/hooks/useAgentStream')
    expect(pollIntervalMs(true)).toBeLessThan(pollIntervalMs(false))
  })

  it('does not poll every few seconds while streaming', async () => {
    const { pollIntervalMs } = await import('../../src/hooks/useAgentStream')
    expect(pollIntervalMs(false)).toBeGreaterThanOrEqual(15_000)
  })

  it('keeps a hidden tab under a cadence that still feels current on return', async () => {
    const { pollIntervalMs } = await import('../../src/hooks/useAgentStream')
    expect(pollIntervalMs(true)).toBeLessThanOrEqual(10_000)
  })
})

import { describe, it, expect } from 'vitest'
import { isReverifiable, probeSettles, REVERIFY_INTERVAL_MS, REVERIFY_MAX_TICKS } from '../../src/lib/reverify'

describe('isReverifiable', () => {
  it('final verdicts are final — re-probing cannot improve on them', () => {
    expect(isReverifiable('serving')).toBe(false)
    expect(isReverifiable('service')).toBe(false)
    expect(isReverifiable('no-preview')).toBe(false)
  })

  it('everything short of verified is worth another reading', () => {
    for (const v of ['unconfirmed', 'not-listening', 'app-error'] as const) {
      expect(isReverifiable(v), v).toBe(true)
    }
  })
})

describe('probeSettles', () => {
  it('mid-boot is not an answer — a settled verdict must not flap to a spinner', () => {
    expect(probeSettles('starting')).toBe(false)
  })

  it('every other reading replaces what is on screen', () => {
    for (const s of ['ready', 'no_page', 'app_error', 'terminal', 'paused', 'unpublished', 'unknown'] as const) {
      expect(probeSettles(s), s).toBe(true)
    }
  })
})

describe('the budget', () => {
  it('is bounded: an abandoned tab must not poll a dead sandbox all afternoon', () => {
    // ~2 minutes of watching, then the manual button carries on.
    expect(REVERIFY_INTERVAL_MS * REVERIFY_MAX_TICKS).toBeLessThanOrEqual(3 * 60_000)
    expect(REVERIFY_MAX_TICKS).toBeGreaterThanOrEqual(5)
  })
})

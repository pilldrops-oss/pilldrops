import { describe, it, expect, beforeEach } from 'vitest'
import { clearThread, describeAge } from '../../src/lib/threadStore'

beforeEach(() => {
  localStorage.clear()
})

describe('clearThread', () => {
  it('removes the local build-conversation cache keys', () => {
    localStorage.setItem('pilldrops.build.thread', '{"at":1,"turns":[]}')
    localStorage.setItem('pilldrops.build.thread.id', 'th_old')
    clearThread()
    expect(localStorage.getItem('pilldrops.build.thread')).toBeNull()
    expect(localStorage.getItem('pilldrops.build.thread.id')).toBeNull()
  })

  it('does not throw when storage is unavailable', () => {
    expect(() => clearThread()).not.toThrow()
  })
})

describe('describeAge', () => {
  const t0 = 1_700_000_000_000
  it.each([
    [t0, 'just now'],
    [t0 - 5 * 60_000, '5m ago'],
    [t0 - 90 * 60_000, '2h ago'],
    [t0 - 50 * 3_600_000, '2d ago'],
  ])('reads naturally', (at, expected) => {
    expect(describeAge(at, t0)).toBe(expected)
  })

  it('never renders a negative age from a clock that moved backwards', () => {
    // Timezone changes and NTP corrections both move the clock backwards.
    expect(describeAge(t0 + 60_000, t0)).toBe('just now')
  })
})

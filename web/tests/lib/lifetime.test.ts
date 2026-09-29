import { describe, it, expect } from 'vitest'
import { LIFETIME_CHOICES, lifetimeToInputs, formatCountdown } from '../../src/lib/lifetime'

describe('lifetime choices', () => {
  it('offers named choices rather than raw seconds', () => {
    expect(LIFETIME_CHOICES.map(c => c.key)).toEqual(['idle', '1h', '8h', 'forever'])
    LIFETIME_CHOICES.forEach(c => expect(c.label).not.toMatch(/^\d+$/))
  })

  it('maps each choice to the timeout/idle pair the API expects', () => {
    expect(lifetimeToInputs('1h')).toEqual({ timeoutInput: '3600', idleInput: '-1' })
    expect(lifetimeToInputs('8h')).toEqual({ timeoutInput: '28800', idleInput: '-1' })
    // "Stop when idle" must not pin a lifetime.
    expect(lifetimeToInputs('idle')).toEqual({ timeoutInput: '', idleInput: '' })
    expect(lifetimeToInputs('forever')).toEqual({ timeoutInput: '-1', idleInput: '-1' })
  })
})

describe('formatCountdown', () => {
  const now = 1_000_000

  it('reads as time remaining, not a timestamp', () => {
    expect(formatCountdown(now + 2520, now)).toBe('42m')
    expect(formatCountdown(now + 7200, now)).toBe('2h 0m')
    expect(formatCountdown(now + 45, now)).toBe('45s')
  })

  it('says so plainly once expired', () => {
    expect(formatCountdown(now - 10, now)).toBe('expired')
  })

  it('returns empty for a sandbox with no expiry', () => {
    expect(formatCountdown(0, now)).toBe('')
    expect(formatCountdown(-1, now)).toBe('')
  })
})

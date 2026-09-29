import { describe, expect, it } from 'vitest'
import { statusPill } from '../../src/pages/Sandboxes'

// The daemon has seven states; only running is a confirmed, listening machine.

const GREEN = '#16A34A'

describe('statusPill', () => {
  it('is green for exactly one state', () => {
    const states = ['pending', 'running', 'pausing', 'paused', 'resuming', 'stopped', 'failed',
      'crashed', 'exited']
    const green = states.filter(s => statusPill(s).fg === GREEN)
    expect(green).toEqual(['running'])
  })

  it('never calls a non-running machine "Running"', () => {
    for (const s of ['pending', 'pausing', 'resuming', 'stopped', 'failed', 'crashed', 'exited']) {
      expect(statusPill(s).label).not.toBe('Running')
    }
  })

  it('does not flatter an unknown state', () => {
    const p = statusPill('provisioning-v2')
    expect(p.fg).not.toBe(GREEN)
    expect(p.label).toBe('provisioning-v2')
  })

  it('reads an absent status as Unknown, not Running', () => {
    const p = statusPill('')
    expect(p.label).toBe('Unknown')
    expect(p.fg).not.toBe(GREEN)
  })

  it('gives an in-flight state a verb the user can wait on', () => {
    expect(statusPill('pending').label).toBe('Starting')
    expect(statusPill('resuming').label).toBe('Waking')
  })
})

import { describe, it, expect } from 'vitest'
import { exitStyleFor } from '../../src/pages/History'

// An uncaught render error unmounts the whole React tree, so one malformed row must not throw.
describe('exitStyleFor', () => {
  it('survives a record with no exit state', () => {
    expect(() => exitStyleFor(undefined)).not.toThrow()
    expect(exitStyleFor(undefined).label).toBe('Destroyed')
    expect(exitStyleFor('').label).toBe('Destroyed')
  })

  it('flags a bad ending', () => {
    for (const s of ['crashed', 'OOMKilled', 'exit-error', 'failed']) {
      expect(exitStyleFor(s).fg).toBe('#DC2626')
    }
  })

  it('treats a clean exit as neutral', () => {
    expect(exitStyleFor('destroyed').fg).toBe('#6B7280')
  })
})

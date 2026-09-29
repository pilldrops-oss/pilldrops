import { describe, it, expect, vi, beforeEach } from 'vitest'
import { track, __setSink } from '../../src/lib/telemetry'

describe('track', () => {
  beforeEach(() => __setSink(null))

  it('forwards the event and its properties to the sink', () => {
    const sink = vi.fn()
    __setSink(sink)
    track('template_search_empty', { query: 'postgres' })
    expect(sink).toHaveBeenCalledWith('template_search_empty', { query: 'postgres' })
  })

  it('never throws when no sink is configured', () => {
    expect(() => track('launch_outcome', { state: 'ready' })).not.toThrow()
  })

  it('swallows sink failures — telemetry must not break the app', () => {
    __setSink(() => { throw new Error('network down') })
    expect(() => track('launch_outcome', { state: 'ready' })).not.toThrow()
  })
})

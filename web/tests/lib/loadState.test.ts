import { describe, it, expect } from 'vitest'
import { loadState } from '../../src/lib/loadState'

describe('loadState', () => {
  it('is loading only before anything has arrived', () => {
    expect(loadState(null, null)).toBe('loading')
  })

  it('is failed when the first fetch errored, so an error can be shown', () => {
    expect(loadState(null, 'network down')).toBe('failed')
  })

  it('is ready once data has arrived', () => {
    expect(loadState([{ id: 'sb-1' }], null)).toBe('ready')
  })

  it('is empty for a successful fetch with nothing in it', () => {
    expect(loadState([], null)).toBe('empty')
  })

  // A stale list beats a blank table, provided the staleness is labelled.
  it('stays ready when a later poll fails but data is still held', () => {
    expect(loadState([{ id: 'sb-1' }], 'network down')).toBe('stale')
  })

  it('reports an empty list that later failed as stale, not as newly empty', () => {
    expect(loadState([], 'network down')).toBe('stale')
  })
})

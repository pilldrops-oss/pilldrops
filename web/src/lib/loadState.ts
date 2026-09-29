// usePoll keeps `data` null on error, so `data == null` alone cannot separate a failed
// first fetch from loading. 'stale' keeps last-known rows when a later poll fails.
export type LoadState = 'loading' | 'failed' | 'stale' | 'empty' | 'ready'

export function loadState<T>(data: T[] | null | undefined, error: unknown): LoadState {
  if (data == null) return error ? 'failed' : 'loading'
  if (error) return 'stale'
  return data.length === 0 ? 'empty' : 'ready'
}

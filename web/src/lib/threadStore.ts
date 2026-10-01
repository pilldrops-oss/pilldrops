// clearThread resets the local build-conversation cache keys so "New" never
// resurfaces a stale transcript left over from a previous session.

const KEY = 'pilldrops.build.thread'
const ID_KEY = 'pilldrops.build.thread.id'

export function clearThread(): void {
  try {
    localStorage.removeItem(KEY)
    localStorage.removeItem(ID_KEY)
  } catch {
    /* nothing to do */
  }
}

/** describeAge frames a timestamp in relative terms; never negative even if the clock moved backwards. */
export function describeAge(at: number, now: number): string {
  const mins = Math.max(0, Math.round((now - at) / 60_000))
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.round(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  return `${Math.round(hrs / 24)}d ago`
}

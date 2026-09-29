import type { Verdict } from './launchVerdict'
import type { PreviewState } from '../types'

// Auto re-verification policy: a non-final launch verdict is re-probed for a
// bounded time while the page is open.

// Re-probing can never improve on these.
const FINAL_VERDICTS: ReadonlySet<Verdict> = new Set<Verdict>(['serving', 'service', 'no-preview'])

/** isReverifiable reports whether the verdict is worth probing again. */
export function isReverifiable(v: Verdict): boolean {
  return !FINAL_VERDICTS.has(v)
}

/**
 * probeSettles reports whether a probe result should replace the verdict on
 * screen. "starting" never does, so a settled verdict does not flap back to a spinner.
 */
export function probeSettles(state: PreviewState): boolean {
  return state !== 'starting'
}

// 10 ticks at 12s covers a normal install; after that the manual "Check again" button carries on.
export const REVERIFY_INTERVAL_MS = 12_000
export const REVERIFY_MAX_TICKS = 10

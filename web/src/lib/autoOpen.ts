import type { PreviewOutcome } from './previewStatus'

// One named decision function so none of the auto-open guards can be quietly dropped from a JSX block.

export type AutoOpenInput = {
  phase: 'form' | 'running' | 'done' | 'error'
  /** Undefined means there was nothing to probe. */
  outcome?: PreviewOutcome
  kind?: string
  visible: boolean
  /** Whether the user has already acted (clicked Open, gone back, launched again). */
  userActed: boolean
}

/** shouldAutoOpen returns false in every ambiguous case; a missed auto-open costs one click. */
export function shouldAutoOpen(i: AutoOpenInput): boolean {
  if (i.phase !== 'done') return false
  if (i.userActed) return false
  // A background tab returning to a replaced launch summary loses its one-shot notes.
  if (!i.visible) return false
  // For an app or service the launch summary is the deliverable; for an agent the Agent tab is.
  if (i.kind !== 'agent') return false
  // Undefined is allowed: an agent workstation has no app to probe, so there was never a verdict.
  if (i.outcome !== undefined && i.outcome !== 'ready') return false
  return true
}

/** AUTO_OPEN_DELAY_MS is long enough to read the notice and cancel, short enough not to feel like a hang. */
export const AUTO_OPEN_DELAY_MS = 1200

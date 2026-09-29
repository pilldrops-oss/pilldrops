import type { PreviewStatus, PreviewState } from '../types'

/** PreviewOutcome is a poll's terminal state; `unreachable` is client-side only (deadline hit while still starting). */
export type PreviewOutcome = PreviewState | 'unreachable'

export interface PollResult {
  state: PreviewOutcome
  /** Last server-reported state, for the timeout case. */
  lastState?: PreviewState
  /** Last upstream HTTP status seen, for diagnosis copy. */
  upstreamStatus?: number
}

export interface PollOpts {
  probe: () => Promise<PreviewStatus>
  sleep: (ms: number) => Promise<void>
  now: () => number
  /** Give up after this long still "starting". */
  deadlineMs: number
  /** Gap between probes; default 2s matches the server's Retry-After. */
  intervalMs?: number
}

/**
 * pollPreviewReady polls until the preview leaves `starting` or the deadline passes.
 * Only `starting` is retried; `app_error` and `unknown` are final answers.
 */
export async function pollPreviewReady(opts: PollOpts): Promise<PollResult> {
  const { probe, sleep, now, deadlineMs, intervalMs = 2_000 } = opts
  const start = now()
  let lastState: PreviewState | undefined
  let upstreamStatus: number | undefined
  let everAnswered = false

  for (;;) {
    try {
      const s = await probe()
      everAnswered = true
      lastState = s.state
      upstreamStatus = s.upstream_status
      if (s.state !== 'starting') {
        return { state: s.state, lastState, upstreamStatus }
      }
    } catch {
      // A failed probe looks like "not up yet"; let the deadline decide.
      lastState = lastState ?? 'starting'
    }

    if (now() - start >= deadlineMs) {
      // "unreachable" blames the user's app, so only claim it if the control
      // plane actually answered at least once.
      return { state: everAnswered ? 'unreachable' : 'unknown', lastState, upstreamStatus }
    }
    await sleep(intervalMs)
  }
}

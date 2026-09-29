import type { PollResult } from './previewStatus'

// An agent workstation has no app port to probe, so readiness comes from the supervisor's
// session state instead.

const READY_STATES = new Set(['idle', 'running', 'awaiting_approval'])

export interface AgentReadyOpts {
  /** Returns the session state, or throws while the supervisor is not answering yet. */
  probe: () => Promise<{ session?: { state?: string } }>
  sleep: (ms: number) => Promise<void>
  now: () => number
  deadlineMs: number
  intervalMs?: number
}

/** pollAgentReady waits for the supervisor to report a usable session; `degraded` counts as app_error, not ready. */
export async function pollAgentReady(o: AgentReadyOpts): Promise<PollResult> {
  const interval = o.intervalMs ?? 1500
  const started = o.now()
  let lastState: string | undefined

  for (;;) {
    try {
      const st = await o.probe()
      lastState = st.session?.state
      if (lastState && READY_STATES.has(lastState)) {
        return { state: 'ready' }
      }
      if (lastState === 'degraded' || lastState === 'stopped') {
        // A definite bad answer; waiting out the deadline would not change it.
        return { state: 'app_error' }
      }
    } catch {
      // Not answering yet; expected while the container boots.
    }
    if (o.now() - started >= o.deadlineMs) {
      return { state: 'unreachable', lastState: lastState as PollResult['lastState'] }
    }
    await o.sleep(interval)
  }
}

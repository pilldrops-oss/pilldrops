import type { CreateSandboxResp, Template } from '../types'
import type { PollResult } from './previewStatus'

// Pure interpretation of observations already made; it never probes, so it cannot invent
// a verdict. Only 'serving' may claim success outright.

export type Verdict =
  | 'serving'
  /** No preview to check; the box itself is the deliverable (agents, shells). */
  | 'no-preview'
  /** Speaks its own protocol, so an HTTP probe can never classify it. */
  | 'service'
  | 'app-error'
  /** Deadline hit while still starting. */
  | 'not-listening'
  | 'unconfirmed'

export interface Verdicted {
  verdict: Verdict
  url?: string
  upstreamStatus?: number
}

/** verifyTarget is the URL worth probing, or '' for services that speak their own protocol. */
export function verifyTarget(resp: CreateSandboxResp, tpl: Template | undefined): string {
  if (tpl?.kind === 'service') return ''
  return resp.viewer_url || resp.app_url || resp.url || ''
}

/** verdictFromPoll maps `unknown` to 'unconfirmed', never to a failure that blames the user's code. */
export function verdictFromPoll(poll: PollResult, url: string): Verdicted {
  const upstreamStatus = poll.upstreamStatus
  switch (poll.state) {
    case 'ready':
      return { verdict: 'serving', url, upstreamStatus }
    case 'app_error':
      return { verdict: 'app-error', url, upstreamStatus }
    case 'unreachable':
      return { verdict: 'not-listening', url, upstreamStatus }
    case 'no_page':
      // Listening, but nothing at the probed path: neither not-listening nor serving.
      return { verdict: 'unconfirmed', url, upstreamStatus }
    default:
      return { verdict: 'unconfirmed', url, upstreamStatus }
  }
}

/** unprobedVerdict is the verdict for a launch that is never probed. */
export function unprobedVerdict(tpl: Template | undefined): Verdicted {
  return { verdict: tpl?.kind === 'service' ? 'service' : 'no-preview' }
}

/** verdictLine is the sentence shown to the user; only 'serving' is phrased as success. */
export function verdictLine(v: Verdicted, agentName: string): string {
  switch (v.verdict) {
    case 'serving':
      return `${agentName} is up and serving.`
    case 'no-preview':
      return `${agentName} is running — open a terminal to use it.`
    case 'service':
      return `${agentName} is running. It speaks its own protocol, so connect with a client rather than a browser.`
    case 'app-error':
      return v.upstreamStatus
        ? `${agentName} started but is returning errors (HTTP ${v.upstreamStatus}). The logs will say why.`
        : `${agentName} started but is returning errors. The logs will say why.`
    case 'not-listening':
      // No guessed cause: the caller quotes the log when it says why.
      return `${agentName} started, but I couldn’t reach it on its port yet.`
    case 'unconfirmed':
      return `${agentName} started. I couldn't confirm it's serving, so check it before relying on it.`
  }
}

/** verdictStatus is the glanceable two-or-three word form for the live panel. */
export function verdictStatus(v: Verdicted | null): string {
  if (v === null) return 'Checking…'
  switch (v.verdict) {
    case 'serving':
      return 'Serving'
    case 'no-preview':
    case 'service':
      return 'Running'
    case 'app-error':
      return v.upstreamStatus ? `Erroring · HTTP ${v.upstreamStatus}` : 'Erroring'
    case 'not-listening':
      return 'Not listening'
    case 'unconfirmed':
      return 'Unconfirmed'
  }
}

/** linkIsUsable is true only for a verified serving verdict; never offer a link known to 502. */
export function linkIsUsable(v: Verdicted): boolean {
  return v.verdict === 'serving' && !!v.url
}

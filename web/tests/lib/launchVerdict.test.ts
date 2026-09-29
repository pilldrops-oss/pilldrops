import { describe, it, expect } from 'vitest'
import {
  verifyTarget,
  verdictFromPoll,
  unprobedVerdict,
  verdictLine,
  verdictStatus,
  linkIsUsable,
} from '../../src/lib/launchVerdict'
import type { CreateSandboxResp, Template } from '../../src/types'
import type { PollResult } from '../../src/lib/previewStatus'

const tpl = (kind: string): Template => ({ id: 't', name: 'T', kind } as Template)
const resp = (o: Partial<CreateSandboxResp> = {}): CreateSandboxResp =>
  ({ id: 'sb-1', template: 't', status: 'running', created_at: '', expires_at: '', ...o } as CreateSandboxResp)
const poll = (state: PollResult['state'], upstreamStatus?: number): PollResult =>
  ({ state, upstreamStatus })

describe('verifyTarget', () => {
  it('prefers the tokened viewer URL, then app_url, then url', () => {
    // Probing the untokened URL of a browser/desktop template would 401.
    expect(verifyTarget(resp({ viewer_url: 'V', app_url: 'A', url: 'U' }), tpl('screen'))).toBe('V')
    expect(verifyTarget(resp({ app_url: 'A', url: 'U' }), tpl('app'))).toBe('A')
    expect(verifyTarget(resp({ url: 'U' }), tpl('app'))).toBe('U')
  })

  it('never probes a service — an HTTP probe cannot classify Postgres', () => {
    // The portal does hand services a preview URL, so a present URL is not permission to probe.
    expect(verifyTarget(resp({ url: 'postgres://x' }), tpl('service'))).toBe('')
  })

  it('returns empty when the template publishes no port at all', () => {
    expect(verifyTarget(resp(), tpl('agent'))).toBe('')
  })
})

describe('verdictFromPoll', () => {
  it('only ready is success', () => {
    expect(verdictFromPoll(poll('ready'), 'U').verdict).toBe('serving')
  })

  it('app_error is up-and-broken, not still-starting', () => {
    const v = verdictFromPoll(poll('app_error', 500), 'U')
    expect(v.verdict).toBe('app-error')
    expect(v.upstreamStatus).toBe(500)
  })

  it('a deadline hit while starting means nothing is listening', () => {
    expect(verdictFromPoll(poll('unreachable'), 'U').verdict).toBe('not-listening')
  })

  it('unknown is OUR uncertainty and must not be reported as the app failing', () => {
    expect(verdictFromPoll(poll('unknown'), 'U').verdict).toBe('unconfirmed')
  })

  it('no_page is listening but unproven — neither success nor "not listening"', () => {
    expect(verdictFromPoll(poll('no_page', 404), 'U').verdict).toBe('unconfirmed')
  })
})

describe('unprobedVerdict', () => {
  it('separates "nothing to serve" from "speaks another protocol"', () => {
    expect(unprobedVerdict(tpl('agent')).verdict).toBe('no-preview')
    expect(unprobedVerdict(tpl('service')).verdict).toBe('service')
  })
})

describe('verdictLine', () => {
  it('claims success in exactly one case', () => {
    const claims = (s: string) => /\bis up and serving\b/.test(s)
    expect(claims(verdictLine({ verdict: 'serving', url: 'U' }, 'Nginx'))).toBe(true)
    for (const v of ['no-preview', 'service', 'app-error', 'not-listening', 'unconfirmed'] as const) {
      expect(claims(verdictLine({ verdict: v }, 'Nginx')), `${v} must not read as serving`).toBe(false)
    }
  })

  it('names the upstream status when it has one, and copes when it does not', () => {
    expect(verdictLine({ verdict: 'app-error', upstreamStatus: 502 }, 'App')).toContain('HTTP 502')
    // No status: must not render "HTTP undefined".
    expect(verdictLine({ verdict: 'app-error' }, 'App')).not.toMatch(/HTTP\s*(undefined|NaN)?\b\s*\)/)
    expect(verdictLine({ verdict: 'app-error' }, 'App')).toContain('returning errors')
  })

  it('does not blame the user when the verdict is our own uncertainty', () => {
    const s = verdictLine({ verdict: 'unconfirmed' }, 'App')
    expect(s).toMatch(/couldn.t confirm/i)
    expect(s).not.toMatch(/exited|not listening/i)
  })

  it('tells a service user to use a client, not a browser', () => {
    expect(verdictLine({ verdict: 'service' }, 'Postgres')).toMatch(/client/i)
  })
})

describe('linkIsUsable', () => {
  it('offers the link only when the preview was verified', () => {
    expect(linkIsUsable({ verdict: 'serving', url: 'U' })).toBe(true)
    expect(linkIsUsable({ verdict: 'app-error', url: 'U' })).toBe(false)
    expect(linkIsUsable({ verdict: 'not-listening', url: 'U' })).toBe(false)
    expect(linkIsUsable({ verdict: 'unconfirmed', url: 'U' })).toBe(false)
  })

  it('is false when there is no url even on success', () => {
    expect(linkIsUsable({ verdict: 'serving' })).toBe(false)
  })
})

describe('verdictStatus', () => {
  it('is short, and never the same string as the full sentence', () => {
    for (const v of ['serving', 'no-preview', 'service', 'app-error', 'not-listening', 'unconfirmed'] as const) {
      const short = verdictStatus({ verdict: v })
      expect(short.length, v).toBeLessThan(24)
      expect(short, v).not.toBe(verdictLine({ verdict: v }, 'App'))
    }
  })

  it('distinguishes not-yet-known from known-good', () => {
    expect(verdictStatus(null)).toMatch(/checking/i)
    expect(verdictStatus({ verdict: 'serving' })).toBe('Serving')
  })

  it('carries the upstream status when there is one', () => {
    expect(verdictStatus({ verdict: 'app-error', upstreamStatus: 500 })).toContain('500')
    expect(verdictStatus({ verdict: 'app-error' })).toBe('Erroring')
  })

  it('never calls an unverified launch "Serving"', () => {
    for (const v of ['app-error', 'not-listening', 'unconfirmed'] as const) {
      expect(verdictStatus({ verdict: v }), v).not.toMatch(/serving/i)
    }
  })
})

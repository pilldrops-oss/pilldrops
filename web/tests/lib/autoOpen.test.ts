import { describe, expect, it } from 'vitest'
import { shouldAutoOpen, AUTO_OPEN_DELAY_MS } from '../../src/lib/autoOpen'

// Not auto-opening costs one click; auto-opening wrongly loses a launch summary the user
// cannot get back. Every case here errs toward not opening.

const ready = {
  phase: 'done' as const,
  outcome: 'ready' as const,
  kind: 'agent',
  visible: true,
  userActed: false,
}

describe('shouldAutoOpen', () => {
  it('opens a ready agent workstation while the user is watching', () => {
    expect(shouldAutoOpen(ready)).toBe(true)
  })

  it('does not open into a background tab', () => {
    expect(shouldAutoOpen({ ...ready, visible: false })).toBe(false)
  })

  it('does not open over the top of something the user did', () => {
    expect(shouldAutoOpen({ ...ready, userActed: true })).toBe(false)
  })

  it('does not open when the probe never confirmed readiness', () => {
    // 'unreachable' means the deadline passed while still starting.
    expect(shouldAutoOpen({ ...ready, outcome: 'unreachable' })).toBe(false)
    expect(shouldAutoOpen({ ...ready, outcome: 'app_error' })).toBe(false)
  })

  it('still opens when there was nothing to probe', () => {
    expect(shouldAutoOpen({ ...ready, outcome: undefined })).toBe(true)
  })

  it('leaves app and service launches on the summary', () => {
    // For those the summary is the deliverable: preview URL, connection string, credentials.
    for (const kind of ['app', 'service', 'browser', 'gui', undefined]) {
      expect(shouldAutoOpen({ ...ready, kind })).toBe(false)
    }
  })

  it('does nothing before the launch finishes', () => {
    for (const phase of ['form', 'running', 'error'] as const) {
      expect(shouldAutoOpen({ ...ready, phase })).toBe(false)
    }
  })

  it('pauses long enough to be cancelled', () => {
    expect(AUTO_OPEN_DELAY_MS).toBeGreaterThanOrEqual(800)
    expect(AUTO_OPEN_DELAY_MS).toBeLessThanOrEqual(3000)
  })
})

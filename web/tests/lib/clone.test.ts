import { describe, it, expect } from 'vitest'
import { cloneSourceFor } from '../../src/lib/clone'
import type { StoredLaunch } from '../../src/lib/launchMemory'
import type { CreateSandboxReq } from '../../src/types'

const body = (template: string): CreateSandboxReq => ({ template }) as CreateSandboxReq

const recents: StoredLaunch[] = [
  { template: 'playwright', name: 'Playwright', at: 200, body: body('playwright') },
  { template: 'fastapi', name: 'FastAPI', at: 100, body: body('fastapi') },
  // Large uploads are stored body-less by launchMemory (MAX_BODY_JSON).
  { template: 'nextjs', name: 'Next.js', at: 300 },
]

describe('cloneSourceFor', () => {
  it('returns the stored launch body when there is one', () => {
    const r = cloneSourceFor(recents, 'playwright')
    expect(r.body?.template).toBe('playwright')
    expect(r.exact).toBe(true)
  })

  it('falls back to template-only when the body was not kept', () => {
    const r = cloneSourceFor(recents, 'nextjs')
    expect(r.body).toBeUndefined()
    expect(r.template).toBe('nextjs')
    expect(r.exact).toBe(false)
  })

  it('falls back to template-only when the template was never launched here', () => {
    const r = cloneSourceFor(recents, 'rust')
    expect(r.body).toBeUndefined()
    expect(r.template).toBe('rust')
    expect(r.exact).toBe(false)
  })

  it('prefers the most recent launch of a template', () => {
    const dupes: StoredLaunch[] = [
      { template: 'go', name: 'Go', at: 100, body: { template: 'go', start: 'old' } as CreateSandboxReq },
      { template: 'go', name: 'Go', at: 900, body: { template: 'go', start: 'new' } as CreateSandboxReq },
    ]
    expect(cloneSourceFor(dupes, 'go').body?.start).toBe('new')
  })
})

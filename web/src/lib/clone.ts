import type { StoredLaunch } from './launchMemory'
import type { CreateSandboxReq } from '../types'

/** `exact` is false when only the template is known (body dropped as oversized, or created outside this browser). */
export interface CloneSource {
  template: string
  body?: CreateSandboxReq
  exact: boolean
}

/** cloneSourceFor finds the most recent remembered launch of a template. */
export function cloneSourceFor(recents: StoredLaunch[], template: string): CloneSource {
  const withBody = recents
    .filter(r => r.template === template && r.body)
    .sort((a, b) => b.at - a.at)

  if (withBody.length > 0) {
    return { template, body: withBody[0].body, exact: true }
  }
  return { template, exact: false }
}

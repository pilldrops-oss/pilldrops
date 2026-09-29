import type { Template } from '../types'

// A plan: one or more services, and how they are wired.

export type ServiceRole = 'app' | 'database' | 'cache' | 'agent' | 'browser' | 'other'

export interface PlannedService {
  /** Stable within the plan ('api', 'db'); links and the UI refer to it. */
  id: string
  role: ServiceRole
  template: Template
  /** Why this service is here, composed from the rule that fired, never generated prose. */
  reason: string
}

export interface ServiceLink {
  /** Service id that receives the variable. */
  from: string
  to: string
  /** The variable injected into `from`, e.g. DATABASE_URL. */
  env: string
  reason: string
}

/** PlanGap is something asked for that the engine cannot supply. */
export interface PlanGap {
  want: string
  why: string
  offer: Array<'paste-repo' | 'upload' | 'start-agent'>
}

export interface Plan {
  services: PlannedService[]
  links: ServiceLink[]
  gaps: PlanGap[]
  /** exact = a named template; derived = composed from capabilities; partial = something unsatisfiable. */
  confidence: 'exact' | 'derived' | 'partial'
}

export function isComposed(p: Plan): boolean {
  return p.services.length > 1
}

export function isEmpty(p: Plan): boolean {
  return p.services.length === 0 && p.gaps.length === 0
}

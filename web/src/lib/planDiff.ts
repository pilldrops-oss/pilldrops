import type { Plan, PlannedService, ServiceLink } from './plan'

// Diff between the plan that ran last time and the one about to run.

export interface ServiceChange {
  id: string
  from: string
  to: string
}

export interface PlanDiff {
  added: PlannedService[]
  removed: PlannedService[]
  /** Same service id, different template. */
  changed: ServiceChange[]
  linksAdded: ServiceLink[]
  linksRemoved: ServiceLink[]
}

const EMPTY: PlanDiff = { added: [], removed: [], changed: [], linksAdded: [], linksRemoved: [] }

// Tolerate a malformed stored snapshot rather than throw.
function servicesOf(p: Plan | null | undefined): PlannedService[] {
  return Array.isArray(p?.services) ? p!.services : []
}
function linksOf(p: Plan | null | undefined): ServiceLink[] {
  return Array.isArray(p?.links) ? p!.links : []
}
const linkKey = (l: ServiceLink) => `${l.from}->${l.to}:${l.env}`

export function diffPlans(before: Plan | null | undefined, after: Plan | null | undefined): PlanDiff {
  // A first run has nothing to interrupt for.
  if (!before) return EMPTY

  const b = new Map(servicesOf(before).map(s => [s.id, s]))
  const a = new Map(servicesOf(after).map(s => [s.id, s]))

  const added = [...a.values()].filter(s => !b.has(s.id))
  const removed = [...b.values()].filter(s => !a.has(s.id))
  const changed: ServiceChange[] = []
  for (const [id, svc] of a) {
    const was = b.get(id)
    if (was && was.template.id !== svc.template.id) {
      changed.push({ id, from: was.template.id, to: svc.template.id })
    }
  }

  const bl = new Map(linksOf(before).map(l => [linkKey(l), l]))
  const al = new Map(linksOf(after).map(l => [linkKey(l), l]))

  return {
    added,
    removed,
    changed,
    linksAdded: [...al.values()].filter(l => !bl.has(linkKey(l))),
    linksRemoved: [...bl.values()].filter(l => !al.has(linkKey(l))),
  }
}

/** hasChanges reports whether the diff is worth showing the user. */
export function hasChanges(d: PlanDiff): boolean {
  return (
    d.added.length > 0 ||
    d.removed.length > 0 ||
    d.changed.length > 0 ||
    d.linksAdded.length > 0 ||
    d.linksRemoved.length > 0
  )
}

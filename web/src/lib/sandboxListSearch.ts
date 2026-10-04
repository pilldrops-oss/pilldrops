import type { SandboxRow } from '../types'

export type SandboxSort = 'recent' | 'name' | 'oldest'

export const SANDBOX_SORT_LABELS: Record<SandboxSort, string> = {
  recent: 'Recent',
  name: 'Name A-Z',
  oldest: 'Oldest first',
}

// showNodeColumn is true once any row carries `node`: the API includes it
// only for operator callers, so a tenant's list never triggers it.
export function showNodeColumn(rows: SandboxRow[]): boolean {
  return rows.some(r => r.node !== undefined)
}

export function matchesSandboxQuery(row: SandboxRow, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  return [row.name ?? '', row.id, row.template].join(' ').toLowerCase().includes(q)
}

const createdMs = (s: SandboxRow) => Date.parse(s.created_at) || 0
const nameOf = (s: SandboxRow) => (s.name || s.id).toLowerCase()
// Running first, then newest: the order the list has always shown.
const recentRank = (s: SandboxRow) => (s.status === 'running' ? 0 : 1)

export function sortSandboxRows(rows: SandboxRow[], sort: SandboxSort): SandboxRow[] {
  const sorted = [...rows]
  if (sort === 'name') {
    sorted.sort((a, b) => nameOf(a).localeCompare(nameOf(b)))
  } else if (sort === 'oldest') {
    sorted.sort((a, b) => createdMs(a) - createdMs(b))
  } else {
    sorted.sort((a, b) => recentRank(a) - recentRank(b) || createdMs(b) - createdMs(a))
  }
  return sorted
}

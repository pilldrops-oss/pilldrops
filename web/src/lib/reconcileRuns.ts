import type { ProjectRun, HistoryRecord } from '../types'

/**
 * closableRuns returns open runs whose every sandbox appears in history. A run
 * with an unknown or live sandbox stays open; a wrong cost is worse than "running".
 */
export function closableRuns(runs: ProjectRun[], history: HistoryRecord[]): string[] {
  const destroyed = new Set(history.map(h => h.id))

  return runs.flatMap(r => {
    if (r.ended_at) return []
    const ids = (r.services ?? []).map(s => s.sandbox_id).filter(Boolean)
    if (ids.length === 0) return []
    if (!ids.every(id => destroyed.has(id))) return []
    return [r.id]
  })
}

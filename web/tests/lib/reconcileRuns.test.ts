import { describe, it, expect } from 'vitest'
import { closableRuns } from '../../src/lib/reconcileRuns'
import type { ProjectRun, HistoryRecord } from '../../src/types'

const run = (id: string, sandboxes: string[], ended?: number): ProjectRun =>
  ({ id, project_id: 'pr-1', org_id: 'o', started_at: 100, ended_at: ended, services: sandboxes.map(s => ({ service_id: s, sandbox_id: s })) })

const hist = (id: string): HistoryRecord =>
  ({ id, org_id: 'o', template: 't', exit_state: 'destroyed', created_at: 1, destroyed_at: 2, duration_secs: 1, vcpu: 1, mem_mib: 512 })

// A run is over when every sandbox it used is gone from the live set and present in history.
describe('closableRuns', () => {
  it('closes a run once every sandbox it used is destroyed', () => {
    const out = closableRuns([run('r1', ['sb-1', 'sb-2'])], [hist('sb-1'), hist('sb-2')])
    expect(out).toEqual(['r1'])
  })

  it('leaves a run open while any of its sandboxes is still alive', () => {
    expect(closableRuns([run('r1', ['sb-1', 'sb-2'])], [hist('sb-1')])).toEqual([])
  })

  it('ignores runs that are already closed', () => {
    expect(closableRuns([run('r1', ['sb-1'], 999)], [hist('sb-1')])).toEqual([])
  })

  // History is paginated, so a sandbox outside the window is not provably finished.
  it('leaves a run open when its sandboxes are not in the history window', () => {
    expect(closableRuns([run('r1', ['sb-old'])], [hist('sb-1')])).toEqual([])
  })

  it('leaves a run with no recorded sandboxes alone', () => {
    expect(closableRuns([run('r1', [])], [hist('sb-1')])).toEqual([])
  })

  it('handles empty inputs', () => {
    expect(closableRuns([], [])).toEqual([])
  })
})

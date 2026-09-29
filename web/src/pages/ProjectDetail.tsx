import { useEffect, useState } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import { getProject, listRuns, listHistory, endRun, listSandboxes } from '../api'
import type { Project, ProjectRun } from '../types'
import type { Plan } from '../lib/plan'
import type { Verdicted, Verdict } from '../lib/launchVerdict'
import { materialisePlan } from '../lib/materialise'
import { closableRuns } from '../lib/reconcileRuns'
import { diffPlans, hasChanges } from '../lib/planDiff'
import { describeAge } from '../lib/threadStore'
import { errText } from '../lib/errText'
import ArchitectureView from '../components/ArchitectureView'
import ErrorState from '../components/ErrorState'
import { RowsSkeleton } from '../components/Skeleton'

function parsePlan(raw: string | undefined): Plan | null {
  if (!raw) return null
  try {
    const p = JSON.parse(raw) as Plan
    return Array.isArray(p?.services) ? p : null
  } catch {
    return null
  }
}

function verdictsOf(run: ProjectRun | undefined): Record<string, Verdicted | null> {
  const out: Record<string, Verdicted | null> = {}
  for (const s of run?.services ?? []) {
    out[s.service_id] = s.verdict ? ({ verdict: s.verdict as Verdict } as Verdicted) : null
  }
  return out
}

function runState(endedAt: number | string | undefined, alive: boolean): string {
  if (endedAt) return 'ended'
  return alive ? 'running' : 'ended — sandboxes gone'
}

// 'past' covers both an explicitly ended run and one whose sandboxes are all
// gone. liveIds is null while the sandbox list is still unknown, and an
// unknown list must not demote a run.
function runPhase(latest: ProjectRun | undefined, liveIds: Set<string> | null): 'proposed' | 'past' | 'live' {
  if (!latest) return 'proposed'
  if (latest.ended_at) return 'past'
  const allGone = liveIds !== null && !(latest.services ?? []).some(s => liveIds.has(s.sandbox_id))
  return allGone ? 'past' : 'live'
}

function RunRow({ run, prev, liveIds }: { run: ProjectRun; prev?: ProjectRun; liveIds: Set<string> | null }) {
  const now = Date.now()
  const services = run.services ?? []
  const diff = diffPlans(parsePlan(prev?.plan_snapshot), parsePlan(run.plan_snapshot))
  const alive = liveIds === null || services.some(s => liveIds.has(s.sandbox_id))
  return (
    <div className="pd-run">
      <div className="pd-run-top">
        <span className="pj-run-when">{describeAge(run.started_at * 1000, now)}</span>
        <span className="pj-run-svcs">
          {services.length === 0 ? '—' : services.map(s => s.service_id).join(' · ')}
        </span>
        <span className="pj-run-cost">{runState(run.ended_at, alive)}</span>
      </div>
      {hasChanges(diff) && (
        <div className="plan-diff pd-run-diff">
          {diff.changed.map(c => (
            <div className="plan-diff-row plan-diff-mod" key={c.id}>~ {c.id}: {c.from} → {c.to}</div>
          ))}
          {diff.added.map(s => (
            <div className="plan-diff-row plan-diff-add" key={s.id}>+ {s.id} · {s.template.name}</div>
          ))}
          {diff.removed.map(s => (
            <div className="plan-diff-row plan-diff-del" key={s.id}>− {s.id} · {s.template.name}</div>
          ))}
        </div>
      )}
    </div>
  )
}

export default function ProjectDetail() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const [project, setProject] = useState<Project | null>(null)
  const [runs, setRuns] = useState<ProjectRun[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [liveIds, setLiveIds] = useState<Set<string> | null>(null)

  useEffect(() => {
    let live = true
    setError(null)
    listSandboxes()
      .then(r => { if (live) setLiveIds(new Set((r.sandboxes ?? []).map(s => s.id))) })
      .catch(() => {})
    Promise.all([getProject(id), listRuns(id)])
      .then(async ([p, r]) => {
        if (!live) return
        setProject(p)
        const list = r.runs ?? []
        setRuns(list)
        if (!list.some(x => !x.ended_at)) return
        const hist = await listHistory(200, 0).catch(() => ({ history: [] }))
        const closable = closableRuns(list, hist.history ?? [])
        if (!closable.length || !live) return
        await Promise.all(closable.map(runId => endRun(id, runId).catch(() => {})))
        const fresh = await listRuns(id)
        if (live) setRuns(fresh.runs ?? [])
      })
      .catch(e => { if (live) setError(errText(e)) })
    return () => { live = false }
  }, [id])

  const plan = parsePlan(project?.plan)
  const latest = runs?.[0]
  const units = plan ? materialisePlan(plan) : []
  const env = units.flatMap(u => Object.entries(u.env))

  if (error) return <div className="wrap"><ErrorState error={error} onRetry={() => navigate(0)} title="Couldn’t load this project" /></div>
  if (!project) return <div className="wrap"><div className="panel"><RowsSkeleton rows={4} /></div></div>

  function runsBody() {
    if (runs === null) return <RowsSkeleton rows={2} />
    if (runs.length === 0) return <div className="empty">No runs yet.</div>
    return (
      <div className="pj-runs">
        {runs.map((r, i) => <RunRow key={r.id} run={r} prev={runs[i + 1]} liveIds={liveIds} />)}
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <p className="pd-back"><Link to="/projects">← Projects</Link></p>
          <h1 className="h1">{project.name}</h1>
          <p>
            {plan
              ? `${plan.services.length} service${plan.services.length === 1 ? '' : 's'} · ${units.length} sandbox${units.length === 1 ? '' : 'es'}`
              : 'No plan stored for this project.'}
          </p>
        </div>
        {plan && (
          <button className="btn primary" onClick={() => navigate('/', { state: { plan, projectId: project.id } })}>
            Run again →
          </button>
        )}
      </div>

      {plan && plan.services.length > 0 && (
        <div className="panel pd-panel">
          <div className="ph"><h2>How it’s wired</h2>
            <span className="meta">{latest ? `last run ${describeAge(latest.started_at * 1000, Date.now())}` : 'never run'}</span>
          </div>
          <ArchitectureView
            plan={plan}
            verdicts={verdictsOf(latest)}
            phase={runPhase(latest, liveIds)}
          />
        </div>
      )}

      {env.length > 0 && (
        <div className="panel pd-panel">
          <div className="ph"><h2>Connection strings</h2><span className="meta">injected at start</span></div>
          <dl className="pd-env">
            {env.map(([k, v]) => (
              <div className="pd-env-row" key={k}>
                <dt>{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}

      <div className="panel pd-panel">
        <div className="ph"><h2>Runs</h2><span className="meta">newest first</span></div>
        {runsBody()}
      </div>
    </div>
  )
}

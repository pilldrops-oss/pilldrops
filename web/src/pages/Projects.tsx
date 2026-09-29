import { useEffect, useState, useRef } from 'react'
import { Link } from 'react-router-dom'
import { listProjects, listRuns, archiveProject, updateProject, listHistory, endRun } from '../api'
import type { Project, ProjectRun } from '../types'
import { describeAge } from '../lib/threadStore'
import { loadState } from '../lib/loadState'
import { closableRuns } from '../lib/reconcileRuns'
import { errText } from '../lib/errText'
import ErrorState from '../components/ErrorState'
import { RowsSkeleton } from '../components/Skeleton'
import { useConfirm } from '../components/ConfirmModal'
import { useToast } from '../components/Toast'

function ageOf(unixSec: number | undefined, now: number): string {
  return unixSec ? describeAge(unixSec * 1000, now) : 'never'
}

function RunRow({ run }: { run: ProjectRun }) {
  const now = Date.now()
  const services = run.services ?? []
  const bad = services.filter(s => s.verdict === 'not-listening' || s.verdict === 'app-error').length
  return (
    <div className="pj-run">
      <span className="pj-run-when">{ageOf(run.started_at, now)}</span>
      <span className="pj-run-svcs">
        {services.length === 0
          ? '—'
          : services.map(s => `${s.service_id}${s.verdict ? ` (${s.verdict})` : ''}`).join(' · ')}
      </span>
      <span className="pj-run-cost">{run.ended_at ? 'ended' : 'running'}</span>
      {bad > 0 && <span className="pj-run-bad">{bad} failed</span>}
    </div>
  )
}

function ProjectCard({ p, onChanged }: { p: Project; onChanged: () => void }) {
  const [runs, setRuns] = useState<ProjectRun[] | null>(null)
  const [open, setOpen] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const [name, setName] = useState(p.name)
  const confirm = useConfirm()
  const toast = useToast()

  useEffect(() => {
    if (!open || runs) return
    let live = true
    listRuns(p.id)
      .then(r => { if (live) setRuns(r.runs ?? []) })
      .catch(() => { if (live) setRuns([]) })
    return () => { live = false }
  }, [open, runs, p.id])

  const reconciled = useRef(false)
  useEffect(() => {
    if (!runs || reconciled.current) return
    if (!runs.some(r => !r.ended_at)) return
    reconciled.current = true
    let live = true
    ;(async () => {
      try {
        const hist = await listHistory(200, 0)
        const closable = closableRuns(runs, hist.history ?? [])
        if (!closable.length) return
        await Promise.all(closable.map(runId => endRun(p.id, runId)))
        const fresh = await listRuns(p.id)
        if (live) setRuns(fresh.runs ?? [])
      } catch {
      }
    })()
    return () => { live = false }
  }, [runs, p.id])

  async function commitRename() {
    const next = name.trim()
    setRenaming(false)
    if (!next || next === p.name) return
    try {
      await updateProject(p.id, { name: next })
      onChanged()
    } catch (e) {
      toast.error(errText(e))
      setName(p.name)
    }
  }

  async function confirmArchive() {
    const ok = await confirm({
      title: 'Archive project?',
      body: <>This hides <b>{p.name}</b> from the list. Its run history is kept.</>,
      confirmLabel: 'Archive',
    })
    if (!ok) return
    try {
      await archiveProject(p.id)
      onChanged()
    } catch (e) {
      toast.error(errText(e))
    }
  }

  function runsBody() {
    if (runs === null) return <p className="build-eg">Loading…</p>
    if (runs.length === 0) return <p className="build-eg">No runs yet.</p>
    return runs.map(r => <RunRow key={r.id} run={r} />)
  }

  return (
    <div className="panel pj-card">
      <div className="pj-head">
        {renaming ? (
          <input
            className="pj-name-in"
            value={name}
            autoFocus
            maxLength={64}
            onChange={e => setName(e.target.value)}
            onBlur={commitRename}
            onKeyDown={e => {
              if (e.key === 'Enter') void commitRename()
              if (e.key === 'Escape') { setName(p.name); setRenaming(false) }
            }}
            aria-label="Project name"
          />
        ) : (
          <>
            <Link className="pj-name" to={`/projects/${p.id}`}>{p.name}</Link>
            <button type="button" className="build-linkish" onClick={() => setRenaming(true)} title="Rename">
              Rename
            </button>
          </>
        )}
        <span className="pj-meta">last run {ageOf(p.last_run_at, Date.now())}</span>
        <button type="button" className="build-linkish" onClick={() => setOpen(o => !o)}>
          {open ? 'Hide runs' : 'Runs'}
        </button>
        <button type="button" className="build-linkish" onClick={confirmArchive}>
          Archive
        </button>
      </div>

      {open && (
        <div className="pj-runs">
          {runsBody()}
        </div>
      )}
    </div>
  )
}

export default function Projects() {
  const [projects, setProjects] = useState<Project[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [unavailable, setUnavailable] = useState(false)

  function load() {
    setError(null)
    listProjects()
      .then(r => setProjects(r.projects ?? []))
      .catch((e: { status?: number }) => {
        if (e?.status === 404) { setUnavailable(true); setProjects([]) }
        else { setError(errText(e)); setProjects(null) }
      })
  }
  useEffect(load, [])

  const state = loadState(projects, error)

  function projectsBody() {
    if (unavailable) return <div className="empty">Projects aren’t enabled on this deployment.</div>
    if (state === 'loading') return <div className="panel"><RowsSkeleton rows={3} /></div>
    if (state === 'failed') return <ErrorState error={error ?? 'Unknown error'} onRetry={load} title="Couldn’t load your projects" />
    if (projects && projects.length === 0) {
      return (
        <div className="empty-hero">
          <h2>No projects yet</h2>
          <p>
            Build something, and once it works you’ll be offered the chance to keep it as a project —
            so you can come back to it and see what each run cost.
          </p>
        </div>
      )
    }
    return (projects ?? []).map(p => <ProjectCard key={p.id} p={p} onChanged={load} />)
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Projects</h1>
          <p>Things you are building. A sandbox is where one ran; a project outlives it.</p>
        </div>
      </div>

      {projectsBody()}
    </div>
  )
}

import { useState, useEffect, useRef, useCallback } from 'react'
import { Link, useNavigate, useLocation } from 'react-router-dom'
import { useSSE } from '../hooks/useSSE'
import { usePoll } from '../hooks/useApi'

import { previewUrl, getSandbox, pauseSandbox, resumeSandbox, destroySandbox, renameSandbox, getUsage, getConcurrency, getEvents, listTemplates, listSandboxes, IS_LOCAL_RUNTIME } from '../api'
import type { StatsPayload, SandboxRow, UsageResp, EventRow, Template } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { MetricsSkeleton, RowsSkeleton } from '../components/Skeleton'
import { errText, isCreditError } from '../lib/errText'
import { loadState } from '../lib/loadState'
import ErrorState from '../components/ErrorState'
import { cloneSourceFor } from '../lib/clone'
import { listRecents } from '../lib/launchMemory'
import { TemplateIcon } from '../components/templateIcons'
import { kindOf } from '../lib/launchKinds'
import { HistoryPanel } from './History'

const CREDIT_ACTION = { label: 'Add credits', to: 'usage' } as const
function actionFor(e: unknown) {
  return isCreditError(e) ? { action: CREDIT_ACTION } : undefined
}

function formatAge(createdAt: string): string {
  const diff = Math.max(0, Date.now() - new Date(createdAt).getTime()) / 1000
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) {
    const m = Math.floor(diff / 60)
    const s = Math.floor(diff % 60)
    return `${m}m ${String(s).padStart(2, '0')}s`
  }
  const h = Math.floor(diff / 3600)
  const m = Math.floor((diff % 3600) / 60)
  return `${h}h ${m}m`
}

interface SparkProps {
  points: number[]
  width?: number
  height?: number
}

function Sparkline({ points, width = 200, height = 26 }: SparkProps) {
  if (points.length < 2) return <svg className="spark" viewBox={`0 0 ${width} ${height}`} />
  const max = Math.max(...points, 1)
  const step = width / (points.length - 1)
  const pts = points.map((v, i) => `${i * step},${height - (v / max) * (height - 2) - 1}`)
  const d = `M${pts.join(' L')}`
  const fill = `M${pts[0]} L${pts.slice(1).join(' L')} L${width},${height} L0,${height} Z`
  return (
    <svg className="spark" viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none">
      <defs>
        <linearGradient id="sg" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="var(--chartink)" stopOpacity=".14" />
          <stop offset="1" stopColor="var(--chartink)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={d} fill="none" stroke="var(--chartink)" strokeWidth="1.6" />
      <path d={fill} fill="url(#sg)" />
    </svg>
  )
}

interface AreaChartProps {
  points: number[]
}

function AreaChart({ points }: AreaChartProps) {
  const W = 620
  const H = 150
  const PAD = 10
  if (points.length < 2) {
    return (
      <svg width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
        <line x1="0" y1="40" x2={W} y2="40" stroke="var(--gridline)" />
        <line x1="0" y1="80" x2={W} y2="80" stroke="var(--gridline)" />
        <line x1="0" y1="120" x2={W} y2="120" stroke="var(--gridline)" />
      </svg>
    )
  }
  const max = Math.max(...points, 1)
  const step = W / (points.length - 1)
  const y = (v: number) => H - PAD - (v / max) * (H - PAD * 2)
  const coords = points.map((v, i) => `${i * step},${y(v)}`)
  const line = coords.join(' ')
  const poly = `${coords.join(' ')} ${W},${H} 0,${H}`

  return (
    <svg width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
      <defs>
        <linearGradient id="ag" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="var(--chartink)" stopOpacity=".10" />
          <stop offset="1" stopColor="var(--chartink)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <line x1="0" y1="40" x2={W} y2="40" stroke="var(--gridline)" />
      <line x1="0" y1="80" x2={W} y2="80" stroke="var(--gridline)" />
      <line x1="0" y1="120" x2={W} y2="120" stroke="var(--gridline)" />
      <polyline points={line} fill="none" stroke="var(--chartink)" strokeWidth="2" />
      <polygon points={poly} fill="url(#ag)" />
      {points.length > 0 && (
        <circle
          cx={(points.length - 1) * step}
          cy={y(points[points.length - 1])}
          r="3.5"
          fill="var(--chartink)"
        />
      )}
    </svg>
  )
}

interface RowMenuProps {
  row: SandboxRow
  onDone: () => void
}

function RowMenu({ row, onDone }: RowMenuProps) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [coords, setCoords] = useState<{ top: number; right: number } | null>(null)
  const ref = useRef<HTMLDivElement>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  const isRunning = row.status === 'running'
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()

  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    const dismiss = () => setOpen(false)
    document.addEventListener('mousedown', onDocClick)
    window.addEventListener('scroll', dismiss, true)
    window.addEventListener('resize', dismiss)
    return () => {
      document.removeEventListener('mousedown', onDocClick)
      window.removeEventListener('scroll', dismiss, true)
      window.removeEventListener('resize', dismiss)
    }
  }, [open])

  function toggle() {
    if (open) { setOpen(false); return }
    const r = btnRef.current?.getBoundingClientRect()
    if (r) setCoords({ top: r.bottom + 6, right: Math.max(8, window.innerWidth - r.right) })
    setOpen(true)
  }

  function cloneRow() {
    const src = cloneSourceFor(listRecents(), row.template)
    setOpen(false)
    if (!src.exact) {
      toast.show(`Original settings for ${row.template} aren’t saved here — starting from the template.`)
    }
    navigate('/deploy', { state: { templateId: src.template, cloneBody: src.body } })
  }

  async function action(fn: () => Promise<unknown>) {
    setBusy(true)
    try { await fn() } catch (e) { toast.error(errText(e), actionFor(e)) }
    setBusy(false)
    setOpen(false)
    onDone()
  }

  async function confirmDestroy() {
    setOpen(false)
    const ok = await confirm({
      title: 'Destroy sandbox?',
      body: <>This permanently destroys <code>{row.id}</code> and its filesystem. This can’t be undone.</>,
      confirmLabel: 'Destroy',
      danger: true,
    })
    if (ok) action(() => destroySandbox(row.id))
  }

  return (
    <div className="menu-wrap" ref={ref}>
      <button
        ref={btnRef}
        className="iconbtn"
        onClick={toggle}
        disabled={busy}
        title="Actions"
        aria-label="Sandbox actions"
      >
        ⋯
      </button>
      {open && coords && (
        <div className="menu menu-fixed" style={{ top: coords.top, right: coords.right }}>
          <button onClick={() => { setOpen(false); navigate(`/sandbox/${row.id}`) }}>Open</button>
          {!IS_LOCAL_RUNTIME && (isRunning ? (
            <button onClick={() => action(() => pauseSandbox(row.id))}>Pause</button>
          ) : (
            <button onClick={() => action(() => resumeSandbox(row.id))}>Resume</button>
          ))}
          <button onClick={cloneRow}>Clone</button>
          <button className="danger" onClick={confirmDestroy}>
            Destroy
          </button>
        </div>
      )}
    </div>
  )
}

type FilterType = 'all' | 'running' | 'paused'
type RangeType = '5m' | '1h' | '1d'

const RANGE_LABEL: Record<RangeType, string> = { '5m': 'last 5 minutes', '1h': 'last hour', '1d': 'last 24 hours' }

function eventVerb(e: EventRow): string {
  const what = e.event === 'created' ? 'launched' : e.event
  return `${e.template || 'sandbox'} ${what}`
}

function eventColor(ev: string): string {
  if (ev === 'created' || ev === 'resumed') return 'var(--green)'
  if (ev === 'destroyed') return '#DC2626'
  return '#B7791F'
}

function agoShort(unixSec: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unixSec))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export default function Sandboxes() {
  const tabParam = new URLSearchParams(useLocation().search).get('tab')
  const [tab, setTab] = useState<'active' | 'past'>(tabParam === 'past' ? 'past' : 'active')
  useEffect(() => {
    if (tabParam === 'past' || tabParam === 'active') setTab(tabParam)
  }, [tabParam])
  const [filter, setFilter] = useState<FilterType>('all')
  const [range, setRange] = useState<RangeType>('5m')
  const navigate = useNavigate()
  const [sparkHistory, setSparkHistory] = useState<number[]>([])
  const [chartHistory, setChartHistory] = useState<number[]>([])
  const [, setTick] = useState(0)

  const { data: stats } = useSSE<StatsPayload>('/dashboard/api/events', 'stats')

  useEffect(() => {
    if (stats?.active_count === undefined) return
    const v = stats.active_count
    setSparkHistory(h => [...h, v].slice(-20))
    setChartHistory(h => [...h, v].slice(-60))
  }, [stats])

  useEffect(() => {
    const id = setInterval(() => setTick(t => t + 1), 1000)
    return () => clearInterval(id)
  }, [])

  // Catalog entries decide whether a row's preview is a desktop; fetched once.
  const [tpls, setTpls] = useState<Template[]>([])
  useEffect(() => {
    let alive = true
    listTemplates().then(ts => { if (alive) setTpls(ts) }).catch(() => {})
    return () => { alive = false }
  }, [])

  const { data: sandboxes, error: sandboxesError, refetch } = usePoll<SandboxRow[]>(
    () => listSandboxes().then(r => r.sandboxes),
    3000
  )

  const { data: usage } = usePoll<UsageResp>(getUsage, 10000)

  const { data: conc } = usePoll(() => getConcurrency(range), 15000, [range])
  const rangePoints = (conc?.points ?? []).map(p => p.n)

  const { data: eventsResp } = usePoll(() => getEvents(50), 5000)
  const events = eventsResp?.events ?? []
  const logsRef = useRef<HTMLDivElement>(null)

  const handleRefetch = useCallback(() => { refetch() }, [refetch])
  const openLaunch = useCallback((templateId?: string) => {
    navigate('/deploy', templateId ? { state: { templateId } } : {})
  }, [navigate])

  const state = loadState(sandboxes, sandboxesError)
  const loading = state === 'loading'

  const total = sandboxes?.length ?? 0
  const running = sandboxes?.filter(s => s.status === 'running').length ?? 0
  const paused = sandboxes?.filter(s => s.status === 'paused').length ?? 0

  const booted = sandboxes?.filter(s => s.boot_ms > 0) ?? []
  const avgBoot = booted.length
    ? Math.round(booted.reduce((a, s) => a + s.boot_ms, 0) / booted.length)
    : 0

  const vcpuH = usage?.vcpu_hours ?? 0
  const ramH = usage?.ram_gb_hours ?? 0

  const filtered = (sandboxes ?? []).filter(s => {
    if (filter === 'running') return s.status === 'running'
    if (filter === 'paused') return s.status !== 'running'
    return true
  })
  const rank = (s: SandboxRow) => (s.status === 'running' ? 0 : 1)
  const createdMs = (s: SandboxRow) => Date.parse(s.created_at) || 0
  filtered.sort((a, b) => rank(a) - rank(b) || createdMs(b) - createdMs(a))

  const maxActive = stats?.max_active ?? 50
  const activeCount = sandboxes ? total : (stats?.active_count ?? running)

  function listBody() {
    if (tab === 'past') return <HistoryPanel />
    if (loading) {
      return (
        <>
          <MetricsSkeleton />
          <div className="panel">
            <RowsSkeleton rows={4} />
          </div>
        </>
      )
    }
    if (state === 'failed') {
      return (
        <ErrorState
          error={sandboxesError ?? 'Unknown error'}
          onRetry={refetch}
          title="Couldn’t load your sandboxes"
        />
      )
    }
    if (total === 0) return <EmptyState onLaunch={openLaunch} />
    return (
        <>
      <div className="metrics">
        <div className="metric">
          <div className="l"><span className="d" /> Concurrent</div>
          <div className="v">
            {activeCount}<u> / {maxActive}</u>
          </div>
          {sparkHistory.length > 1 && (
            <div className="chg" style={{ color: 'var(--green)' }}>
              ↑ {activeCount}
            </div>
          )}
          <Sparkline points={sparkHistory} />
        </div>
        <div className="metric">
          <div className="l">vCPU-hours</div>
          <div className="v">{vcpuH > 0 ? vcpuH.toFixed(2) : '—'}</div>
        </div>
        <div className="metric">
          <div className="l">RAM GB-hrs</div>
          <div className="v">{ramH > 0 ? ramH.toFixed(2) : '—'}</div>
        </div>
        <div className="metric">
          <div className="l">Boot p50</div>
          <div className="v">
            {avgBoot > 0 ? avgBoot : '—'}{avgBoot > 0 && <u>ms</u>}
          </div>
        </div>
      </div>
      <div className="panel">
          <div className="ph">
            <h2>Active</h2>
            <div className="rgt">
              <div className="filter">
                {(['all', 'running', 'paused'] as FilterType[]).map(f => (
                  <button
                    type="button"
                    key={f}
                    className={filter === f ? 'on' : ''}
                    onClick={() => setFilter(f)}
                    style={{ textTransform: 'capitalize' }}
                  >
                    {f === 'all' ? 'All' : f.charAt(0).toUpperCase() + f.slice(1)}
                  </button>
                ))}
              </div>
              <span className="meta">{state === 'stale' ? 'reconnecting…' : 'auto · 3s'}</span>
            </div>
          </div>
          <div className="sb-table-scroll">
            <div className="rowh">
              <div>Sandbox</div>
              <div>Template</div>
              <div>Status</div>
              <div>Port</div>
              <div>Age</div>
              <div />
            </div>
            {filtered.length === 0 ? (
              <div className="empty">No {filter === 'all' ? '' : `${filter} `}sandboxes match this filter.</div>
            ) : (
              filtered.map(row => (
                <SandboxRowItem key={row.id} row={row} template={tpls.find(t => t.id === row.template)} onDone={handleRefetch} />
              ))
            )}
          </div>
        </div>
      <div className="two">
        <div className="panel chartwrap">
          <div className="ph">
            <h3>Concurrent sandboxes</h3>
            <div className="filter">
              {(['5m', '1h', '1d'] as RangeType[]).map(rr => (
                <button
                  key={rr}
                  type="button"
                  className={range === rr ? 'on' : ''}
                  onClick={() => setRange(rr)}
                >{rr}</button>
              ))}
            </div>
          </div>
          <div className="sub">{RANGE_LABEL[range]}</div>
          <AreaChart points={rangePoints.length > 1 ? rangePoints : chartHistory} />
        </div>
        <div className="panel term-panel">
          <div className="ph">
            <h2>Activity</h2>
            <span className="meta">live</span>
          </div>
          <div className="logs" ref={logsRef}>
            {events.length === 0 ? (
              <span style={{ color: 'var(--faint)' }}>No recent activity.</span>
            ) : (
              events.map((e, i) => (
                <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '3px 0', fontSize: 12.5 }}>
                  <span style={{ width: 7, height: 7, borderRadius: '50%', background: eventColor(e.event), flex: 'none' }} />
                  <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {eventVerb(e)} <span style={{ color: 'var(--dim)', fontFamily: 'var(--mono)' }}>{e.sandbox_id}</span>
                  </span>
                  <span style={{ color: 'var(--faint)', fontFamily: 'var(--mono)', flex: 'none' }}>{agoShort(e.at)}</span>
                </div>
              ))
            )}
          </div>
        </div>
      </div>
        </>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Sandboxes</h1>
          <p>
            {total} sandbox{total !== 1 ? 'es' : ''} ·{' '}
            <b>{running} running</b> ·{' '}
            <em>{paused} paused</em>
          </p>
        </div>
        <button className="btn primary" onClick={() => openLaunch()}>
          ＋ Launch sandbox
        </button>
      </div>

      <div className="tab-bar" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'active'}
          className={`tab-item${tab === 'active' ? ' active' : ''}`}
          onClick={() => setTab('active')}
        >
          Active
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'past'}
          className={`tab-item${tab === 'past' ? ' active' : ''}`}
          onClick={() => setTab('past')}
        >
          Past
        </button>
      </div>

      {listBody()}
    </div>
  )
}

function EmptyState({ onLaunch }: { onLaunch: (templateId?: string) => void }) {
  const navigate = useNavigate()
  const [tpls, setTpls] = useState<Template[]>([])

  useEffect(() => {
    let alive = true
    listTemplates().then(ts => { if (alive) setTpls(ts.slice(0, 6)) }).catch(() => {})
    return () => { alive = false }
  }, [])

  return (
    <div className="empty-hero">
      <div className="empty-hero-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24">
          <rect x="3" y="3" width="7" height="7" rx="1.5" />
          <rect x="14" y="3" width="7" height="7" rx="1.5" />
          <rect x="3" y="14" width="7" height="7" rx="1.5" />
          <rect x="14" y="14" width="7" height="7" rx="1.5" />
        </svg>
      </div>
      <h2>Launch your first sandbox</h2>
      <p>
        Spin up a disposable{IS_LOCAL_RUNTIME ? '' : ', isolated'} machine in seconds — pick a template, run code
        or a web app, and get a live preview URL.
      </p>
      <div className="empty-hero-cta">
        <button className="btn primary" onClick={() => onLaunch()}>＋ Launch sandbox</button>
        <button className="btn" onClick={() => navigate('/deploy')}>Deploy an app</button>
        <button className="btn" onClick={() => navigate('/docs')}>Read the docs</button>
      </div>
      {tpls.length > 0 && (
        <>
          <div className="empty-hero-or">or start from a template</div>
          <div className="empty-hero-tpls">
            {tpls.map(t => (
              <button key={t.id} className="empty-tpl" onClick={() => onLaunch(t.id)} title={t.description || t.name || t.id}>
                <span className="empty-tpl-ic"><TemplateIcon t={t} /></span>
                <span className="empty-tpl-nm">{t.name || t.id}</span>
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  )
}

export function statusPill(status: string): { label: string; bg: string; fg: string } {
  switch (status) {
    case 'crashed':
    case 'failed':
      return { label: 'Crashed', bg: '#FEE2E2', fg: '#DC2626' }
    case 'exited':
    case 'stopped':
      return { label: status === 'stopped' ? 'Stopped' : 'Exited', bg: '#F3F4F6', fg: '#6B7280' }
    case 'paused':
      return { label: 'Paused', bg: '#FEF3C7', fg: '#D97706' }
    // Standby is frozen in RAM, not written to disk: it wakes in milliseconds
    // and its own colour says it is nearer running than paused.
    case 'standby':
      return { label: 'Standby', bg: '#DBEAFE', fg: '#2563EB' }
    case 'pending':
      return { label: 'Starting', bg: '#FEF3C7', fg: '#D97706' }
    case 'pausing':
      return { label: 'Pausing', bg: '#FEF3C7', fg: '#D97706' }
    case 'resuming':
      return { label: 'Waking', bg: '#FEF3C7', fg: '#D97706' }
    case 'running':
      return { label: 'Running', bg: '#DCFCE7', fg: '#16A34A' }
    default:
      return { label: status || 'Unknown', bg: '#F3F4F6', fg: '#6B7280' }
  }
}

export interface InlineRenameProps {
  id: string
  name: string
  onRenamed: (name: string) => void
}

export function InlineRename({ id, name, onRenamed }: InlineRenameProps) {
  const [editing, setEditing] = useState(false)
  const [value, setValue] = useState(name)
  const [busy, setBusy] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const toast = useToast()

  useEffect(() => {
    if (editing) inputRef.current?.focus()
  }, [editing])

  function startEdit(e: React.MouseEvent) {
    e.stopPropagation()
    setValue(name)
    setEditing(true)
  }

  function cancel() {
    setValue(name)
    setEditing(false)
  }

  async function commit() {
    const next = value.trim()
    if (next === name) {
      setEditing(false)
      return
    }
    setBusy(true)
    try {
      const updated = await renameSandbox(id, next)
      setEditing(false)
      onRenamed(updated.name ?? next)
      toast.success(updated.name ? `Renamed to ${updated.name}` : 'Name cleared')
    } catch (e) {
      toast.error(errText(e))
    } finally {
      setBusy(false)
    }
  }

  if (editing) {
    return (
      <div className="nm sb-namewrap" onClick={e => e.stopPropagation()}>
        <input
          ref={inputRef}
          className="sb-name-input"
          value={value}
          disabled={busy}
          maxLength={64}
          aria-label="Sandbox name"
          onChange={e => setValue(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') {
              e.preventDefault()
              void commit()
            } else if (e.key === 'Escape') {
              e.preventDefault()
              cancel()
            }
          }}
          onBlur={cancel}
        />
      </div>
    )
  }

  return (
    <div className="nm sb-namewrap">
      <Link className="mono sb-id sb-id-link" to={`/sandbox/${id}`} onClick={e => e.stopPropagation()}>
        {name || id}
      </Link>
      {name && <small className="sb-id-sub mono">{id}</small>}
      <button
        type="button"
        className="sb-rename-btn"
        onClick={e => {
          e.stopPropagation()
          startEdit(e)
        }}
        title="Rename"
        aria-label="Rename sandbox"
      >
        ✎
      </button>
    </div>
  )
}

// Exported for the row-label test. template is the row's catalog entry, when known.
export function SandboxRowItem({ row, template, onDone }: { row: SandboxRow; template?: Template; onDone: () => void }) {
  // A GUI template's preview is its desktop; everything else previews an app.
  const isDesktop = !!template && kindOf(template) === 'gui'
  // Only an explicit false greys it: a node too old to report the field omits
  // it, and treating absent as false would disable a button that works.
  const noDesktop = isDesktop && row.has_desktop === false
  let previewTitle = 'Open preview'
  if (noDesktop) previewTitle = 'This template has no desktop'
  else if (isDesktop) previewTitle = 'Desktop'
  async function openPreview(r: SandboxRow) {
    const w = window.open('', '_blank')
    if (w) w.opener = null
    try {
      const token = r.cdp_token ?? (await getSandbox(r.id)).cdp_token
      const url =
        previewUrl(r.id, r.public_port || 8000) + (token ? `?token=${encodeURIComponent(token)}` : '')
      if (w) w.location.href = url
      else window.location.href = url
    } catch (e) {
      w?.close()
      toast.error(errText(e))
    }
  }

  const status = row.status || 'running'
  const isRunning = status === 'running'
  const isCrashed = status === 'crashed' || status === 'failed'
  const p = statusPill(status)
  const navigate = useNavigate()
  const toast = useToast()

  const handleRowClick = () => {
    navigate(`/sandbox/${row.id}`, { state: { sandbox: row } })
  }

  return (
    <div
      className={`row${isRunning ? '' : ' paused'}`}
      onClick={handleRowClick}
      style={{ cursor: 'pointer', ...(isCrashed ? { borderLeft: '3px solid #DC2626', background: 'rgba(220,38,38,0.04)' } : {}) }}
    >
      <div className="sb">
        <div className="ic"><TemplateIcon template={row.template} /></div>
        <InlineRename id={row.id} name={row.name ?? ''} onRenamed={onDone} />
      </div>
      <div>
        <span className="chip">{row.template}</span>
      </div>
      <div>
        <span className="pill" style={{ background: p.bg, color: p.fg }}>
          <span className="pd" style={{ background: p.fg }} />
          {p.label}
        </span>
      </div>
      <div className={`port${isRunning ? '' : ' off'}`}>
        {isRunning ? `:${row.public_port || 8000}` : '—'}
      </div>
      <div className="age">{formatAge(row.created_at)}</div>
      <div className="rowact" onClick={e => e.stopPropagation()}>
        {isRunning && (
          <button
            className="iconbtn"
            title={previewTitle}
            aria-label={previewTitle}
            disabled={noDesktop}
            onClick={() => void openPreview(row)}
          >
            ↗
          </button>
        )}
        {!isRunning && (
          <button
            className="iconbtn"
            title="Resume"
            onClick={() => resumeSandbox(row.id).then(onDone).catch(e => toast.error(errText(e), actionFor(e)))}
          >
            ▸
          </button>
        )}
        <RowMenu row={row} onDone={onDone} />
      </div>
    </div>
  )
}

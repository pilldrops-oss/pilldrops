import { useEffect, useState } from 'react'
import { listHistory, getHistory } from '../api'
import type { HistoryRecord } from '../types'

function fmtTime(sec: number): string {
  if (!sec) return '—'
  return new Date(sec * 1000).toLocaleString([], {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

function fmtDuration(secs: number): string {
  if (secs <= 0) return '0s'
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const s = secs % 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

export function exitStyleFor(state?: string): { label: string; bg: string; fg: string } {
  const s = (state ?? '').toLowerCase()
  if (s.includes('crash') || s.includes('fail') || s.includes('error') || s.includes('oom')) {
    return { label: state || 'Crashed', bg: '#FEE2E2', fg: '#DC2626' }
  }
  return { label: state || 'Destroyed', bg: '#F3F4F6', fg: '#6B7280' }
}

function HistoryRow({ rec }: { rec: HistoryRecord }) {
  const [open, setOpen] = useState(false)
  const [logs, setLogs] = useState<string | null>(null)
  const [loadingLogs, setLoadingLogs] = useState(false)
  const [logErr, setLogErr] = useState('')
  const badge = exitStyleFor(rec.exit_state)

  async function toggle() {
    const next = !open
    setOpen(next)
    if (next && logs === null && !loadingLogs) {
      setLoadingLogs(true)
      try {
        const full = await getHistory(rec.id)
        setLogs(full.logs ?? '')
      } catch (e) {
        setLogErr(String(e))
      } finally {
        setLoadingLogs(false)
      }
    }
  }

  function logsBody() {
    if (loadingLogs) return <div style={{ color: 'var(--dim)', fontFamily: 'var(--mono)', fontSize: 12 }}>Loading logs…</div>
    if (logErr) return <div style={{ color: '#dc2626', fontFamily: 'var(--mono)', fontSize: 12 }}>Failed to load logs: {logErr}</div>
    if (logs && logs.length > 0) {
      return (
        <div style={{
          fontFamily: 'var(--mono)', fontSize: 12.5, lineHeight: 1.55, whiteSpace: 'pre-wrap',
          maxHeight: 360, overflowY: 'auto', background: 'var(--bg2)', borderRadius: 8, padding: '10px 12px',
        }}>{logs}</div>
      )
    }
    return <div style={{ color: 'var(--dim)', fontFamily: 'var(--mono)', fontSize: 12 }}>No logs were retained for this sandbox.</div>
  }

  return (
    <div className="panel" style={{ marginBottom: 10 }}>
      <button
        onClick={toggle}
        aria-expanded={open}
        style={{
          all: 'unset', cursor: 'pointer', display: 'flex', alignItems: 'center',
          gap: 12, padding: '12px 16px', width: '100%', boxSizing: 'border-box',
        }}
      >
        <span style={{
          fontSize: 11.5, fontWeight: 700, padding: '2px 9px', borderRadius: 20,
          background: badge.bg, color: badge.fg, whiteSpace: 'nowrap',
        }}>{badge.label}</span>

        <div className="hist-id">
          <span style={{ fontSize: 14, fontWeight: 600 }}>{rec.template || rec.image || 'sandbox'}</span>
          <span style={{ fontSize: 12, color: 'var(--dim)', fontFamily: 'var(--mono)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {rec.id}
          </span>
        </div>

        <div className="hist-chips">
          <span className="chip">{fmtDuration(rec.duration_secs)}</span>
          {rec.node ? <span className="chip">{rec.node}</span> : null}
          {rec.vcpu ? <span className="chip">{rec.vcpu} vCPU</span> : null}
          {rec.mem_mib ? <span className="chip">{rec.mem_mib} MB</span> : null}
          <span className="chip" title="destroyed">{fmtTime(rec.destroyed_at)}</span>
        </div>

        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="2"
          style={{ color: 'var(--dim)', transform: open ? 'rotate(180deg)' : 'none', transition: 'transform .15s' }}>
          <path d="M6 9l6 6 6-6" />
        </svg>
      </button>

      {open && (
        <div className="hist-detail">
          <div className="hist-meta with-actions">
            <span>Created <b>{fmtTime(rec.created_at)}</b></span>
            <span>Destroyed <b>{fmtTime(rec.destroyed_at)}</b></span>
            <span>Ran for <b>{fmtDuration(rec.duration_secs)}</b></span>
            {rec.template && (
              <a
                className="btn ghost hist-meta-btn"
                href={`#/deploy?template=${encodeURIComponent(rec.template)}`}
                title="Open the launch form pre-configured with this template"
              >
                ↻ Relaunch template
              </a>
            )}
          </div>
          {rec.vcpu_seconds != null && (
            <div className="hist-meta">
              <span>vCPU·s <b>{(rec.vcpu_seconds || 0).toFixed(1)}</b></span>
              <span>RAM GB·s <b>{(rec.ram_gb_seconds || 0).toFixed(1)}</b></span>
            </div>
          )}
          {logsBody()}
        </div>
      )}
    </div>
  )
}

const PAGE = 25

export function HistoryPanel() {
  const [recs, setRecs] = useState<HistoryRecord[]>([])
  const [loadErr, setLoadErr] = useState('')
  const [limit, setLimit] = useState(PAGE)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    listHistory(limit)
      .then(d => setRecs(d.history ?? []))
      .catch(e => setLoadErr(String(e)))
      .finally(() => setLoading(false))
  }, [limit])

  const hasMore = !loading && recs.length >= limit

  function historyBody() {
    if (loadErr && recs.length === 0) return <div className="panel"><div className="empty">Failed to load history: {loadErr}</div></div>
    if (loading && recs.length === 0) return <div className="panel"><div className="empty">Loading…</div></div>
    if (recs.length === 0) return <div className="panel"><div className="empty">No past sandboxes yet. Destroyed sandboxes appear here with their retained logs.</div></div>
    return (
      <>
        <div>{recs.map(r => <HistoryRow key={r.id} rec={r} />)}</div>
        {hasMore && (
          <div className="hist-more">
            <button className="btn" onClick={() => setLimit(l => l + PAGE)}>Load more</button>
          </div>
        )}
      </>
    )
  }

  return (
    <>
      <p className="sb-tab-note">
        Sandboxes that have been destroyed — what ran, how long, and the logs they left behind.
      </p>

      {historyBody()}
    </>
  )
}

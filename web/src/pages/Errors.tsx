import { Fragment, useState } from 'react'
import { getAdminErrors, getAdminHistory } from '../api'
import type { HistoryRecord } from '../types'
import { RowsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'
import { useApi } from '../hooks/useApi'

const ago = (unix: number) => {
  if (!unix) return '—'
  const s = Math.max(0, Math.floor(Date.now() / 1000) - unix)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}
const ts = (unix: number) => (unix ? new Date(unix * 1000).toLocaleString() : '—')

type LogState = HistoryRecord | 'loading' | 'error' | undefined

function LogDetail({ log }: { log: LogState }) {
  if (log === 'loading') return <div className="err-loading">Loading logs…</div>
  if (log === 'error' || !log) return <div className="err-loading">Couldn’t load logs for this sandbox.</div>
  return (
    <div className="err-detail">
      <div className="err-meta">
        <span>node <b>{log.node || '—'}</b></span>
        <span>duration <b>{log.duration_secs}s</b></span>
        <span>vcpu <b>{log.vcpu}</b></span>
        <span>mem <b>{log.mem_mib} MiB</b></span>
        <span>created <b>{ts(log.created_at)}</b></span>
        <span>destroyed <b>{ts(log.destroyed_at)}</b></span>
      </div>
      <pre className="err-logs">{log.logs?.trim() || '(no logs retained for this sandbox)'}</pre>
    </div>
  )
}

export default function Errors() {
  const { data: rows, error: err, refetch } = useApi(() => getAdminErrors(200).then(r => r.errors))
  const [q, setQ] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const [logs, setLogs] = useState<Record<string, LogState>>({})

  function toggle(id: string) {
    if (open === id) { setOpen(null); return }
    setOpen(id)
    if (!logs[id]) {
      setLogs(m => ({ ...m, [id]: 'loading' }))
      getAdminHistory(id)
        .then(rec => setLogs(m => ({ ...m, [id]: rec })))
        .catch(() => setLogs(m => ({ ...m, [id]: 'error' })))
    }
  }

  if (err) {
    return <div className="wrap"><ErrorState error={err} onRetry={refetch} title="Couldn’t load errors" /></div>
  }

  const query = q.trim().toLowerCase()
  const filtered = (rows ?? []).filter(r =>
    !query || `${r.id} ${r.org_id} ${r.template} ${r.exit_state}`.toLowerCase().includes(query))

  function failures() {
    if (!rows) return <RowsSkeleton rows={5} />
    if (filtered.length === 0) return <div className="empty">{q ? 'No failures match this filter.' : 'No crashed sandboxes. 🎉'}</div>
    return (
      <table className="tbl">
        <thead><tr><th>Sandbox</th><th>Owner</th><th>Template</th><th>State</th><th>When</th></tr></thead>
        <tbody>
          {filtered.map(r => {
            const log = logs[r.id]
            return (
              <Fragment key={r.id}>
                <tr style={{ cursor: 'pointer' }} onClick={() => toggle(r.id)}>
                  <td className="mono">{open === r.id ? '▾ ' : '▸ '}{r.id}</td>
                  <td className="mono">{r.org_id}</td>
                  <td>{r.template || '—'}</td>
                  <td><span className="tag-err">{r.exit_state}</span></td>
                  <td>{ago(r.destroyed_at)}</td>
                </tr>
                {open === r.id && (
                  <tr className="err-logrow">
                    <td colSpan={5}>
                      <LogDetail log={log} />
                    </td>
                  </tr>
                )}
              </Fragment>
            )
          })}
        </tbody>
      </table>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div><h1 className="h1">Errors</h1><p>Crashed &amp; failed sandboxes across the fleet — click a row for its logs</p></div>
        <input
          className="inp"
          placeholder="Filter id / owner / template…"
          value={q}
          onChange={e => setQ(e.target.value)}
          style={{ maxWidth: 240 }}
        />
      </div>

      <div className="panel">
        <div className="ph"><h2>Failures</h2><span className="meta">{rows ? filtered.length : ''}</span></div>
        {failures()}
      </div>
    </div>
  )
}

import { useEffect, useState, type ReactNode } from 'react'
import { useParams, Link } from 'react-router-dom'
import { adminSandbox, previewUrl } from '../api'
import type { AdminSandboxDetail, AdminActivityRow } from '../types'

const ts = (unix?: number) => (unix ? new Date(unix * 1000).toLocaleString() : '—')

function describe(a: AdminActivityRow): string {
  if (!a.detail) return ''
  const d = a.detail
  switch (a.action) {
    case 'sandbox.create': return `template=${d.template ?? '?'}${d.image ? ` image=${d.image}` : ''}${d.client ? ` via ${d.client}` : ''}`
    case 'sandbox.exec':
    case 'sandbox.exec_stream': return `${d.cmd ?? ''}  → exit ${d.exit_code ?? '?'}${d.duration_ms != null ? ` (${d.duration_ms}ms)` : ''}`
    case 'sandbox.start': return `${d.cmd ?? ''}${d.port ? `  :${d.port}` : ''}`
    case 'sandbox.fs.write': return `${d.path ?? ''}${d.size != null ? ` (${d.size} B)` : ''}`
    case 'sandbox.fs.move': return `${d.src ?? ''} → ${d.dst ?? ''}`
    case 'sandbox.fs.mkdir':
    case 'sandbox.fs.remove': return d.path ?? ''
    case 'sandbox.destroy': return d.reason ?? ''
    case 'sandbox.timeout': return d.timeout_secs != null ? `${d.timeout_secs}s` : ''
    default: return ''
  }
}

export default function AdminSandbox() {
  const { id } = useParams<{ id: string }>()
  const [d, setD] = useState<AdminSandboxDetail | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    if (!id) return
    let alive = true
    setD(null)
    setErr('')
    adminSandbox(id).then(next => { if (alive) setD(next) }).catch(e => { if (alive) setErr(String(e)) })
    return () => { alive = false }
  }, [id])

  useEffect(() => {
    if (!id || d?.state !== 'active') return
    let alive = true
    const interval = setInterval(() => {
      adminSandbox(id).then(next => { if (alive) setD(next) }).catch(() => {})
    }, 5000)
    return () => { alive = false; clearInterval(interval) }
  }, [id, d?.state])

  if (err) return <div className="wrap"><div className="empty">Failed to load sandbox: {err}</div></div>
  if (!d) return <div className="wrap"><div className="empty">Loading…</div></div>

  const r = d.record
  const launch = d.launch
  const activity = d.activity ?? []
  const active = d.state === 'active'

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">{id}</h1>
          <p className="mono">
            {d.state === 'unknown'
              ? 'record expired — timeline from audit log'
              : `${r?.template ?? ''} · ${active ? (r?.state ?? 'active') : (r?.exit_state ?? 'destroyed')}` +
                `${r?.source ? ` · ${r.source}` : ''}${r?.node ? ` · ${r.node}` : ''}`}
          </p>
        </div>
        <div>
          {active && r && (
            <a className="btn" href={previewUrl(r.id, r.public_port || 8000)} target="_blank" rel="noreferrer">
              Open preview ↗
            </a>
          )}{' '}
          {r?.org_id && <Link to={`/users/${encodeURIComponent(r.org_id)}`} className="btn">← User</Link>}
        </div>
      </div>

      <Section title="Launch config">
        {launch ? (
          <div className="stats">
            <div><span className="stat-cap">Template</span><b>{launch.template || '—'}</b></div>
            <div><span className="stat-cap">Image</span><b>{launch.image || '—'}</b></div>
            <div><span className="stat-cap">Resources</span><b>{launch.vcpu || '—'} vCPU / {launch.mem_mb || '—'} MB</b></div>
            <div><span className="stat-cap">Ports</span><b>{launch.ports?.length ? launch.ports.join(', ') : (launch.port || '—')}</b></div>
            <div><span className="stat-cap">Start</span><b className="mono">{launch.start_cmd || '—'}</b></div>
            <div><span className="stat-cap">Env keys</span><b className="mono">{launch.env_keys?.join(', ') || '—'}</b></div>
            <div><span className="stat-cap">Files</span><b>{launch.files?.length ? launch.files.map(f => f.path).join(', ') : '—'}</b></div>
            <div><span className="stat-cap">Client</span><b>{launch.client || '—'}</b></div>
          </div>
        ) : (
          <div className="empty">No launch record (created before activity tracking)</div>
        )}
      </Section>

      <Section title={`Activity timeline (${activity.length})`}>
        {activity.length === 0 ? <div className="empty">None</div> : (
          <table className="tbl">
            <thead><tr><th>When</th><th>Action</th><th>Detail</th></tr></thead>
            <tbody>
              {activity.map((a, i) => (
                <tr key={i}>
                  <td>{ts(a.at)}</td>
                  <td className="mono">{a.action.replace('sandbox.', '')}</td>
                  <td className="mono">{describe(a)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Section>

      <Section title={active ? 'Logs (live tail)' : 'Logs (retained)'}>
        {d.logs?.trim()
          ? <pre className="err-logs">{d.logs}</pre>
          : <div className="empty">{d.logs_note || 'No logs captured'}</div>}
      </Section>
    </div>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="panel">
      <div className="ph"><h2>{title}</h2></div>
      {children}
    </div>
  )
}

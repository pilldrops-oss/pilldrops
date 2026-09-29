import { useEffect, useState, type ReactNode } from 'react'
import { useParams, Link } from 'react-router-dom'
import { getAdminUser } from '../api'
import type { AdminUserDetail } from '../types'
import { Bars, toBars } from '../components/charts'

const ts = (unix: number) => (unix ? new Date(unix * 1000).toLocaleString() : '—')

export default function UserDetail() {
  const { ownerId } = useParams<{ ownerId: string }>()
  const [d, setD] = useState<AdminUserDetail | null>(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    if (!ownerId) return
    getAdminUser(ownerId).then(setD).catch(e => setErr(String(e)))
  }, [ownerId])

  if (err) return <div className="wrap"><div className="empty">Failed to load user: {err}</div></div>
  if (!d) return <div className="wrap"><div className="empty">Loading…</div></div>

  const u = d.user
  const active = d.active ?? []
  const history = d.history ?? []
  const functions = d.functions ?? []
  const events = d.events ?? []
  const audit = d.audit ?? []
  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">{u.email || u.owner_id}</h1>
          <p className="mono">{u.owner_id}</p>
        </div>
        <Link to="/users" className="btn">← All users</Link>
      </div>

      <div className="panel">
        <div className="ph"><h2>Overview</h2></div>
        <div className="stats">
          <div><span className="stat-cap">First seen</span><b>{ts(u.first_seen)}</b></div>
          <div><span className="stat-cap">Last seen</span><b>{ts(u.last_seen)}</b></div>
          <div><span className="stat-cap">Logins</span><b>{u.login_count}</b></div>
          <div><span className="stat-cap">Plan</span><b>{u.plan || 'free'}</b></div>
        </div>
      </div>

      <Section title="Created via">
        <Bars data={toBars(d.by_client)} />
      </Section>

      <Section title={`Active sandboxes (${active.length})`}>
        <Table head={['ID', 'Template', 'State', 'Created']}
          rows={active.map(s => [<Link key={s.id} to={`/sandboxes/${encodeURIComponent(s.id)}`}>{s.id}</Link>, s.template, s.state, ts(s.created_at)])} />
      </Section>

      <Section title={`History (${history.length})`}>
        <Table head={['ID', 'Template', 'Exit', 'Cost', 'Destroyed']}
          rows={history.map(h => [<Link key={h.id} to={`/sandboxes/${encodeURIComponent(h.id)}`}>{h.id}</Link>, h.template, h.exit_state, ts(h.destroyed_at)])} />
      </Section>

      <Section title={`Functions (${functions.length})`}>
        <Table head={['Name', 'Template', 'Invokes', 'Cold starts']}
          rows={functions.map(f => [f.Name, f.Template, String(f.Invokes), String(f.ColdStarts)])} />
      </Section>

      <Section title={`Activity (${events.length})`}>
        <Table head={['Event', 'Sandbox', 'When']} rows={events.map(e => [e.event, e.sandbox_id, ts(e.at)])} />
      </Section>

      <Section title={`Recent actions (${audit.length})`}>
        <Table head={['Action', 'Target', 'When']} rows={audit.map(a => [a.action, a.target, ts(a.at)])} />
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

function Table({ head, rows }: { head: string[]; rows: ReactNode[][] }) {
  if (rows.length === 0) return <div className="empty">None</div>
  return (
    <table className="tbl">
      <thead><tr>{head.map(h => <th key={h}>{h}</th>)}</tr></thead>
      <tbody>{rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j}>{c}</td>)}</tr>)}</tbody>
    </table>
  )
}

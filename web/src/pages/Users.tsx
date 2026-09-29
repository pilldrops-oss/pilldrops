import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { listAdminUsers } from '../api'
import type { AdminUser } from '../types'

const ago = (unix: number) => {
  if (!unix) return '—'
  const s = Math.max(0, Math.floor(Date.now() / 1000) - unix)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export default function Users() {
  const [users, setUsers] = useState<AdminUser[] | null>(null)
  const [err, setErr] = useState('')
  const [q, setQ] = useState('')
  const navigate = useNavigate()

  useEffect(() => {
    listAdminUsers().then(d => setUsers(d.users ?? [])).catch(e => setErr(String(e)))
  }, [])

  const rows = useMemo(() => {
    if (!users) return []
    const needle = q.trim().toLowerCase()
    return needle ? users.filter(u => u.email.toLowerCase().includes(needle)) : users
  }, [users, q])

  if (err) return <div className="wrap"><div className="empty">Failed to load users: {err}</div></div>
  if (!users) return <div className="wrap"><div className="empty">Loading…</div></div>

  return (
    <div className="wrap">
      <div className="h1row">
        <div><h1 className="h1">Users</h1><p>Every signed-in user and what they've created ({users.length} total)</p></div>
        <input className="inp" placeholder="Search email…" value={q} onChange={e => setQ(e.target.value)} />
      </div>
      <div className="panel">
        <table className="tbl">
          <thead>
            <tr>
              <th>Email</th><th>Last seen</th><th>Logins</th><th>Active</th>
              <th>Created</th><th>Functions</th><th>Plan</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(u => (
              <tr
                key={u.owner_id}
                style={{ cursor: 'pointer' }}
                tabIndex={0}
                role="button"
                onClick={() => navigate(`/users/${encodeURIComponent(u.owner_id)}`)}
                onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); navigate(`/users/${encodeURIComponent(u.owner_id)}`) } }}
              >
                <td>{u.email || <span className="mono">{u.owner_id}</span>}</td>
                <td>{ago(u.last_seen)}</td>
                <td>{u.login_count}</td>
                <td>{u.active_sandboxes}</td>
                <td>{u.total_created}</td>
                <td>{u.functions}</td>
                <td>{u.plan || 'free'}</td>
              </tr>
            ))}
            {rows.length === 0 && <tr><td colSpan={8} className="empty">No matching users</td></tr>}
          </tbody>
        </table>
      </div>
    </div>
  )
}

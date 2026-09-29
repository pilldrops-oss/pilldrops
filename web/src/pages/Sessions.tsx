
import { useNavigate } from 'react-router-dom'
import { getAdminSessions } from '../api'

import { MetricsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'
import { DayChart, Kpi } from '../components/charts'
import { usePoll } from '../hooks/useApi'

const ago = (unix: number) => {
  if (!unix) return '—'
  const s = Math.max(0, Math.floor(Date.now() / 1000) - unix)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}
const when = (unix: number) => (unix ? new Date(unix * 1000).toLocaleString() : '—')

export default function Sessions() {
  const { data: d, error: err, refetch } = usePoll(() => getAdminSessions(30), 20000)
  const navigate = useNavigate()

  if (err && !d) {
    return <div className="wrap"><ErrorState error={err} onRetry={refetch} title="Couldn’t load sessions" /></div>
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div><h1 className="h1">Sessions</h1><p>Every sign-in across the fleet · live</p></div>
      </div>

      {!d ? (
        <MetricsSkeleton count={4} />
      ) : (
        <>
          <div className="metrics">
            <Kpi label="Total logins" value={d.total} />
            <Kpi label="Logins · 24h" value={d.logins_24h} />
            <Kpi label="Logins · 7d" value={d.logins_7d} />
            <Kpi label="Unique users" value={d.unique_users} />
          </div>

          <div className="panel">
            <div className="ph"><h2>Logins per day</h2><span className="meta">last 30 days</span></div>
            <div className="ov-body"><DayChart data={d.by_day} empty="No logins recorded yet — sign-ins appear here as they happen." /></div>
          </div>

          <div className="panel">
            <div className="ph"><h2>Recent sign-ins</h2><span className="meta">{d.recent.length}</span></div>
            {d.recent.length === 0 ? (
              <div className="empty">No sign-ins yet.<p>Login events are recorded from now on as users sign in.</p></div>
            ) : (
              <table className="tbl">
                <thead><tr><th>User</th><th>Owner</th><th>When</th></tr></thead>
                <tbody>
                  {d.recent.map((l, i) => (
                    <tr key={i} style={{ cursor: 'pointer' }} onClick={() => navigate(`/users/${encodeURIComponent(l.owner_id)}`)}>
                      <td>{l.email || <span className="mono">{l.owner_id}</span>}</td>
                      <td className="mono">{l.owner_id}</td>
                      <td title={when(l.at)}>{ago(l.at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </>
      )}
    </div>
  )
}

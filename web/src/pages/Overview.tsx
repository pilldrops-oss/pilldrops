import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { getAdminOverview } from '../api'

import { MetricsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'
import { DayChart, Bars, Kpi } from '../components/charts'
import { usePoll } from '../hooks/useApi'

const ago = (unix: number) => {
  if (!unix) return '—'
  const s = Math.max(0, Math.floor(Date.now() / 1000) - unix)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

const crashy = (s: string) => /crash|fail|error|oom/i.test(s)

function eventColor(ev: string): string {
  if (ev === 'created' || ev === 'resumed') return 'var(--green)'
  if (ev === 'destroyed') return '#DC2626'
  return '#B7791F'
}

const RANGES = [7, 14, 30]

export default function Overview() {
  const [days, setDays] = useState(14)
  const { data: d, error: err, refetch } = usePoll(() => getAdminOverview(days), 20000, [days])
  const navigate = useNavigate()

  if (err && !d) {
    return <div className="wrap"><ErrorState error={err} onRetry={refetch} title="Couldn’t load analytics" /></div>
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div><h1 className="h1">Overview</h1><p>Fleet-wide analytics across every user and sandbox · live</p></div>
        <div className="filter">
          {RANGES.map(r => (
            <button type="button" key={r} className={days === r ? 'on' : ''} onClick={() => setDays(r)}>{r}d</button>
          ))}
        </div>
      </div>

      {!d ? (
        <MetricsSkeleton count={8} />
      ) : (
        <>
          <div className="metrics">
            <Kpi label="Users" value={d.total_users} sub={`${d.active_users_24h} active · 24h`} />
            <Kpi label="Logins" value={d.logins} />
            <Kpi label="Active sandboxes" value={d.active_sandboxes} />
            <Kpi label="Total created" value={d.total_created} />
            <Kpi label="Functions" value={d.total_functions} />
            <Kpi label="Crashed" value={d.crashed_count} danger={d.crashed_count > 0} />
            <Kpi label="Templates" value={d.templates.length} />
          </div>

          <div className="panel">
            <div className="ph"><h2>Sandboxes created</h2><span className="meta">last {days} days</span></div>
            <div className="ov-body"><DayChart data={d.created_by_day} empty="No sandboxes created in this window." /></div>
          </div>

          <div className="ov-two">
            <div className="panel">
              <div className="ph"><h2>Created by</h2><span className="meta">agent = CLI/MCP/SDK · human = web</span></div>
              <div className="ov-body"><Bars data={d.by_source} /></div>
            </div>
            <div className="panel">
              <div className="ph"><h2>Top templates</h2><span className="meta">most used</span></div>
              <div className="ov-body"><Bars data={d.templates} /></div>
            </div>
          </div>

          <div className="panel">
            <div className="ph"><h2>Exit states</h2></div>
            <div className="ov-body"><Bars data={d.exit_states} redFor={crashy} /></div>
          </div>

          <div className="panel">
            <div className="ph"><h2>Recent errors</h2><span className="meta">{d.recent_errors.length}</span></div>
            {d.recent_errors.length === 0 ? (
              <div className="empty">No crashed sandboxes. 🎉</div>
            ) : (
              <table className="tbl">
                <thead><tr><th>Sandbox</th><th>Owner</th><th>Template</th><th>State</th><th>When</th></tr></thead>
                <tbody>
                  {d.recent_errors.map(e => (
                    <tr key={e.id} style={{ cursor: 'pointer' }} onClick={() => navigate(`/users/${encodeURIComponent(e.org_id)}`)}>
                      <td className="mono">{e.id}</td>
                      <td className="mono">{e.org_id}</td>
                      <td>{e.template || '—'}</td>
                      <td><span className="tag-err">{e.exit_state}</span></td>
                      <td>{ago(e.destroyed_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>

          <div className="panel">
            <div className="ph"><h2>Recent activity</h2><span className="meta">live fleet</span></div>
            {d.recent_activity.length === 0 ? (
              <div className="empty">No recent activity.</div>
            ) : (
              <table className="tbl">
                <thead><tr><th>Event</th><th>Sandbox</th><th>Owner</th><th>Template</th><th>When</th></tr></thead>
                <tbody>
                  {d.recent_activity.map((a, i) => (
                    <tr key={i} style={{ cursor: 'pointer' }} onClick={() => navigate(`/users/${encodeURIComponent(a.org_id)}`)}>
                      <td><span className="evt" style={{ color: eventColor(a.event) }}>{a.event}</span></td>
                      <td className="mono">{a.sandbox_id}</td>
                      <td className="mono">{a.org_id}</td>
                      <td>{a.template || '—'}</td>
                      <td>{ago(a.at)}</td>
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

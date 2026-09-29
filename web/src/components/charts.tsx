import type { DayCount, NameCount } from '../types'

export function toBars(rec?: Record<string, number>): NameCount[] {
  if (!rec) return []
  return Object.entries(rec)
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => b.count - a.count)
}

export function Kpi({ label, value, sub, danger }: { label: string; value: number | string; sub?: string; danger?: boolean }) {
  return (
    <div className="metric">
      <div className="l">{label}</div>
      <div className="v" style={danger ? { color: '#dc2626' } : undefined}>{value}</div>
      {sub && <div className="ov-sub">{sub}</div>}
    </div>
  )
}

const dayLabel = (day: number) =>
  new Date(day * 86400 * 1000).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })

export function DayChart({ data, empty = 'No data in this window.' }: { data: DayCount[]; empty?: string }) {
  if (data.length === 0) return <div className="empty">{empty}</div>
  const max = Math.max(...data.map(x => x.count), 1)
  return (
    <div className="daychart">
      {data.map(x => (
        <div className="daybar" key={x.day} title={`${dayLabel(x.day)}: ${x.count}`}>
          <span className="daybar-fill" style={{ height: `${Math.max(4, (x.count / max) * 110)}px` }} />
          <span className="daybar-x">{dayLabel(x.day)}</span>
        </div>
      ))}
    </div>
  )
}

export function Bars({ data, redFor }: { data: NameCount[]; redFor?: (name: string) => boolean }) {
  if (data.length === 0) return <div className="empty">No data yet.</div>
  const max = Math.max(...data.map(x => x.count), 1)
  return (
    <div className="bars">
      {data.map(x => (
        <div className="bar-row" key={x.name}>
          <span className="bar-label" title={x.name}>{x.name}</span>
          <span className="bar-track">
            <span
              className="bar-fill"
              style={{ width: `${(x.count / max) * 100}%`, background: redFor?.(x.name) ? '#dc2626' : 'var(--chartink)' }}
            />
          </span>
          <b className="bar-count">{x.count}</b>
        </div>
      ))}
    </div>
  )
}

import { useState } from 'react'
import { useApi } from '../hooks/useApi'
import { getUsage, getConcurrency } from '../api'
import type { UsagePoint } from '../types'
import { Bars, toBars } from '../components/charts'
import { errText } from '../lib/errText'

function TimeChart({ points }: { points: UsagePoint[] }) {
  const W = 620
  const H = 150
  const PAD = 10

  const values = points.map(p => p.concurrent)
  if (points.length < 2 || values.every(v => v === 0)) {
    return (
      <svg width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label="No activity in this range">
        <line x1="0" y1="40" x2={W} y2="40" stroke="var(--gridline)" />
        <line x1="0" y1="80" x2={W} y2="80" stroke="var(--gridline)" />
        <line x1="0" y1="120" x2={W} y2="120" stroke="var(--gridline)" />
        <text x={W / 2} y={H / 2} textAnchor="middle" fill="var(--dim)" fontSize="12" fontFamily="var(--mono)">
          No activity in this range
        </text>
      </svg>
    )
  }

  const max = Math.max(...values, 1)
  const step = W / (points.length - 1)
  const y = (v: number) => H - PAD - (v / max) * (H - PAD * 2)
  const coords = values.map((v, i) => `${i * step},${y(v)}`)
  const line = coords.join(' ')
  const poly = `${coords.join(' ')} ${W},${H} 0,${H}`

  return (
    <div className="usage-chart">
    <div className="usage-chart-scale" aria-label={`Scale from 0 to ${max} sandboxes`}><span>{max}</span><span>0</span></div>
    <svg width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label={`Concurrent sandboxes, peak ${max}`}>
      <defs>
        <linearGradient id="ug" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="var(--chartink)" stopOpacity=".10" />
          <stop offset="1" stopColor="var(--chartink)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <line x1="0" y1="40" x2={W} y2="40" stroke="var(--gridline)" />
      <line x1="0" y1="80" x2={W} y2="80" stroke="var(--gridline)" />
      <line x1="0" y1="120" x2={W} y2="120" stroke="var(--gridline)" />
      <polyline points={line} fill="none" stroke="var(--chartink)" strokeWidth="2" />
      <polygon points={poly} fill="url(#ug)" />
      {values.length > 0 && (
        <circle
          cx={(values.length - 1) * step}
          cy={y(values[values.length - 1])}
          r="3.5"
          fill="var(--chartink)"
        />
      )}
    </svg>
    </div>
  )
}

export default function Usage() {
  const { data, loading, error, refetch } = useApi(getUsage)
  const byClient = toBars(data?.by_client)

  const metrics = [
    { label: 'Total Starts', value: data?.total_starts ?? 0, unit: '' },
    { label: 'vCPU-hours', value: data?.vcpu_hours?.toFixed(2) ?? '—', unit: '' },
    { label: 'RAM GB-hrs', value: data?.ram_gb_hours?.toFixed(2) ?? '—', unit: '' },
  ]

  const [range, setRange] = useState<'1h' | '1d' | '7d'>('1d')
  const { data: conc } = useApi(() => getConcurrency(range), [range])

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Usage</h1>
          <p>Where your hours and storage are going</p>
        </div>
        <button className="btn" onClick={refetch}>Refresh</button>
      </div>

      {error != null && (
        <div className="callout error" style={{ marginBottom: 20 }}>
          <strong>Failed to load usage</strong>
          <pre>{errText(error)}</pre>
        </div>
      )}

      {loading && !data ? (
        <div className="empty">Loading usage data…</div>
      ) : (
        <>
          <div className="field-label spend-details-h">The units behind the number</div>
          <div className={`metrics${metrics.length === 3 ? ' units-3' : ''}`}>
            {metrics.map(m => (
              <div key={m.label} className="metric">
                <div className="l">{m.label}</div>
                <div className="v">{m.value}{m.unit && <u>{m.unit}</u>}</div>
              </div>
            ))}
          </div>

          <div className="panel">
            <div className="ph">
              <h2>Concurrent sandboxes</h2>
              <div className="filter">
                {(['1h', '1d', '7d'] as const).map(r => (
                  <button key={r} type="button" className={range === r ? 'on' : ''} onClick={() => setRange(r)}>
                    {r}
                  </button>
                ))}
              </div>
            </div>
            <div className="chartwrap">
              <TimeChart points={(conc?.points ?? []).map(p => ({ time: p.at, concurrent: p.n, start_rate: 0 }))} />
              {(conc?.points?.length ?? 0) > 1 && (
                <div style={{ marginTop: 10, fontSize: 12, fontFamily: 'var(--mono)', color: 'var(--dim)', display: 'flex', gap: 24 }}>
                  <span>From: {new Date(conc!.points[0].at * 1000).toLocaleString()}</span>
                  <span>To: {new Date(conc!.points[conc!.points.length - 1].at * 1000).toLocaleString()}</span>
                </div>
              )}
            </div>
          </div>

          {(data?.points?.length ?? 0) > 0 && (
            <div className="panel" style={{ marginTop: 16 }}>
              <div className="ph">
                <h2>Start rate</h2>
                <span className="meta">sandboxes / minute · last 60s</span>
              </div>
              <div className="chartwrap">
                <TimeChart
                  points={(data?.points ?? []).map(p => ({
                    ...p,
                    concurrent: p.start_rate,
                  }))}
                />
              </div>
            </div>
          )}

          {byClient.length > 0 && (
            <div className="panel" style={{ marginTop: 16 }}>
              <div className="ph">
                <h2>Created via</h2>
                <span className="meta">cli · mcp · sdk · web · api</span>
              </div>
              <div className="chartwrap">
                <Bars data={byClient} />
              </div>
            </div>
          )}

        </>
      )}
    </div>
  )
}

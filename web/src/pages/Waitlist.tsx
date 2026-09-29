import { useMemo, useState } from 'react'
import { getAdminWaitlist } from '../api'

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
const when = (unix: number) => (unix ? new Date(unix * 1000).toLocaleString() : '—')

export default function Waitlist() {
  const { data: rows, error: err, refetch } = useApi(() => getAdminWaitlist(1000).then(r => r.waitlist))
  const [q, setQ] = useState('')

  const filtered = useMemo(() => {
    const query = q.trim().toLowerCase()
    return (rows ?? []).filter(r => !query || `${r.email} ${r.source} ${r.note}`.toLowerCase().includes(query))
  }, [rows, q])

  if (err) {
    return <div className="wrap"><ErrorState error={err} onRetry={refetch} title="Couldn’t load the waitlist" /></div>
  }

  const border = '1px solid rgba(128,128,128,.22)'
  const dim = 'var(--dim, #8b8b8b)'

  function rowsBody() {
    if (rows === null) return <RowsSkeleton />
    if (filtered.length === 0) {
      return (
        <div style={{ padding: '48px 0', textAlign: 'center', color: dim }}>
          {rows.length === 0 ? 'No signups yet — "Notify me" submissions from pricing and billing will show up here.' : 'No matches.'}
        </div>
      )
    }
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {filtered.map(r => (
          <div key={r.email} style={{ border, borderRadius: 12, padding: '14px 18px', display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
            <a href={`mailto:${r.email}`} style={{ fontWeight: 600, color: '#3ecf8e', textDecoration: 'none' }}>{r.email}</a>
            <span style={{
              fontFamily: 'var(--mono, monospace)', fontSize: 11, fontWeight: 700, letterSpacing: '.04em',
              textTransform: 'uppercase', color: '#7dc4ff', border: '1px solid #7dc4ff44', borderRadius: 6, padding: '2px 8px',
            }}>{r.source || 'unknown'}</span>
            {r.note && <span style={{ color: dim, fontSize: 13 }}>{r.note}</span>}
            <span style={{ marginLeft: 'auto', color: dim, fontSize: 12.5 }} title={when(r.created_at)}>{ago(r.created_at)}</span>
          </div>
        ))}
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Waitlist</h1>
          <p>"Notify me" signups from pricing &amp; billing{rows ? ` · ${rows.length}` : ''}</p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            value={q}
            onChange={e => setQ(e.target.value)}
            placeholder="Search email, source…"
            style={{ padding: '8px 12px', borderRadius: 9, border, background: 'transparent', color: 'inherit', fontSize: 14, minWidth: 240 }}
          />
          <button
            onClick={refetch}
            style={{ padding: '8px 14px', borderRadius: 9, border, background: 'transparent', color: 'inherit', cursor: 'pointer', fontSize: 14 }}
          >Refresh</button>
        </div>
      </div>

      {rowsBody()}
    </div>
  )
}

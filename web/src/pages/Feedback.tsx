import { useMemo, useState } from 'react'
import { getAdminFeedback } from '../api'

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

const KIND_COLOR: Record<string, string> = {
  'bug report': '#e0645c',
  'feature request': '#3ecf8e',
  'pricing / plans': '#ffb03a',
  feedback: '#7dc4ff',
}

export default function Feedback() {
  const { data: rows, error: err, refetch } = useApi(() => getAdminFeedback(300).then(r => r.feedback))
  const [q, setQ] = useState('')

  const filtered = useMemo(() => {
    const query = q.trim().toLowerCase()
    return (rows ?? []).filter(r =>
      !query || `${r.name} ${r.email} ${r.kind} ${r.message}`.toLowerCase().includes(query))
  }, [rows, q])

  if (err) {
    return <div className="wrap"><ErrorState error={err} onRetry={refetch} title="Couldn’t load feedback" /></div>
  }

  const border = '1px solid rgba(128,128,128,.22)'
  const dim = 'var(--dim, #8b8b8b)'

  function rowsBody() {
    if (rows === null) return <RowsSkeleton />
    if (filtered.length === 0) {
      return (
        <div style={{ padding: '48px 0', textAlign: 'center', color: dim }}>
          {rows.length === 0 ? 'No feedback yet — submissions from the contact page will show up here.' : 'No matches.'}
        </div>
      )
    }
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {filtered.map(r => (
          <div key={r.id} style={{ border, borderRadius: 12, padding: '16px 18px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 8 }}>
              <span style={{
                fontFamily: 'var(--mono, monospace)', fontSize: 11, fontWeight: 700, letterSpacing: '.04em',
                textTransform: 'uppercase', color: KIND_COLOR[r.kind] ?? '#7dc4ff',
                border: `1px solid ${KIND_COLOR[r.kind] ?? '#7dc4ff'}44`, borderRadius: 6, padding: '2px 8px',
              }}>{r.kind || 'feedback'}</span>
              <span style={{ fontWeight: 600 }}>{r.name || 'Anonymous'}</span>
              {r.email && <span style={{ color: dim, fontSize: 13 }}>· {r.email}</span>}
              <span style={{ marginLeft: 'auto', color: dim, fontSize: 12.5 }} title={when(r.ts)}>{ago(r.ts)}</span>
            </div>
            <div style={{ whiteSpace: 'pre-wrap', lineHeight: 1.6, fontSize: 14.5 }}>{r.message}</div>
            <div style={{ display: 'flex', gap: 14, marginTop: 10, alignItems: 'center' }}>
              {r.email && (
                <a
                  href={`mailto:${r.email}?subject=${encodeURIComponent(`Re: your PillDrops ${r.kind || 'message'}`)}`}
                  style={{ fontSize: 13, fontWeight: 600, color: '#3ecf8e', textDecoration: 'none' }}
                >Reply by email →</a>
              )}
              {r.source && <span style={{ color: dim, fontSize: 12, fontFamily: 'var(--mono, monospace)' }}>{r.source}</span>}
            </div>
          </div>
        ))}
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Feedback</h1>
          <p>Contact-form submissions{rows ? ` · ${rows.length}` : ''}</p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            value={q}
            onChange={e => setQ(e.target.value)}
            placeholder="Search name, email, message…"
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

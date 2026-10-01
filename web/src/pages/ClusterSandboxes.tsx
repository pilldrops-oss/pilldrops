import { usePoll } from '../hooks/useApi'
import { listSandboxes } from '../api'
import type { SandboxRow } from '../types'
import { showNodeColumn } from '../lib/sandboxListSearch'

function formatAge(createdAt: string): string {
  const diff = Math.max(0, Date.now() - new Date(createdAt).getTime()) / 1000
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) {
    const m = Math.floor(diff / 60)
    const s = Math.floor(diff % 60)
    return `${m}m ${String(s).padStart(2, '0')}s`
  }
  const h = Math.floor(diff / 3600)
  const m = Math.floor((diff % 3600) / 60)
  return `${h}h ${m}m`
}

export default function ClusterSandboxes() {
  const { data } = usePoll<{ sandboxes: SandboxRow[] }>(listSandboxes, 3000, [])
  const rows = data?.sandboxes ?? []
  const running = rows.filter(r => r.status === 'running').length
  const hasNode = showNodeColumn(rows)
  const cols = hasNode ? '1.7fr 0.9fr 1fr 1fr 0.65fr' : '1.7fr 0.9fr 1fr 0.65fr'

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Cluster Sandboxes</h1>
          <p>
            {rows.length} sandbox{rows.length !== 1 ? 'es' : ''} · <b>{running} running</b> · across all nodes · auto · 3s
          </p>
        </div>
      </div>

      <div className="panel">
        <div className="rowh" style={{ gridTemplateColumns: cols }}>
          <div>Sandbox</div>
          <div>Status</div>
          <div>Template</div>
          {hasNode && <div>Node</div>}
          <div>Age</div>
        </div>

        {rows.length === 0 ? (
          <div className="empty">No sandboxes running in the cluster.</div>
        ) : (
          rows.map(row => {
            const isRunning = row.status === 'running'
            return (
              <div
                key={row.id}
                className={`row${isRunning ? '' : ' paused'}`}
                style={{ gridTemplateColumns: cols, cursor: 'default' }}
              >
                <div className="sb">
                  <div className="nm">
                    {row.id}
                    <small>{row.template}</small>
                  </div>
                </div>
                <div>
                  <span className={`pill ${isRunning ? 'run' : 'pau'}`}>
                    <span className="pd" />
                    {isRunning ? 'Running' : (row.status || 'Paused')}
                  </span>
                </div>
                <div>
                  <span className="chip">{row.template}</span>
                </div>
                {hasNode && (
                  <div style={{ fontFamily: 'var(--mono)', fontSize: 12.5, color: 'var(--sub)' }}>
                    {row.node || '—'}
                  </div>
                )}
                <div className="age">{formatAge(row.created_at)}</div>
              </div>
            )
          })
        )}
      </div>
    </div>
  )
}

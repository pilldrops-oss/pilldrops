import { useState } from 'react'
import { useToast } from '../components/Toast'
import { errText } from '../lib/errText'
import { usePoll } from '../hooks/useApi'
import { listNodes, getClusterStats, cordonNode, uncordonNode, drainNode, removeNode } from '../api'
import type { NodeInfo, ClusterStats } from '../types'
import MeterBar from '../components/MeterBar'

function statusStyle(status: string): { bg: string; fg: string; line: string; label: string } {
  switch (status) {
    case 'available':
      return { bg: 'var(--greensoft)', fg: 'var(--green)', line: 'var(--greenline)', label: 'available' }
    case 'full':
      return { bg: 'rgba(217,119,6,.13)', fg: '#d97706', line: 'rgba(217,119,6,.28)', label: 'full' }
    case 'cordoned':
      return { bg: 'var(--slatesoft)', fg: 'var(--slate)', line: 'var(--slateline)', label: 'cordoned' }
    case 'draining':
      return { bg: 'var(--slatesoft)', fg: 'var(--slate)', line: 'var(--slateline)', label: 'draining' }
    case 'unreachable':
      return { bg: 'rgba(220,38,38,.12)', fg: '#dc2626', line: 'rgba(220,38,38,.28)', label: 'unreachable' }
    default:
      return { bg: 'var(--slatesoft)', fg: 'var(--slate)', line: 'var(--slateline)', label: status || 'unknown' }
  }
}

function recStyle(level: string): { bg: string; fg: string; line: string } {
  switch (level) {
    case 'critical':
      return { bg: 'rgba(220,38,38,.12)', fg: '#dc2626', line: 'rgba(220,38,38,.28)' }
    case 'warn':
      return { bg: 'rgba(217,119,6,.13)', fg: '#d97706', line: 'rgba(217,119,6,.28)' }
    default:
      return { bg: 'var(--greensoft)', fg: 'var(--green)', line: 'var(--greenline)' }
  }
}

function StatusPill({ status }: { status: string }) {
  const s = statusStyle(status)
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', gap: 6, borderRadius: 8, padding: '3px 10px',
      fontSize: 12, fontWeight: 600, background: s.bg, color: s.fg, border: `1px solid ${s.line}`,
    }}>
      <span style={{ width: 6, height: 6, borderRadius: '50%', background: 'currentColor' }} />
      {s.label}
    </span>
  )
}

interface NodeCardProps {
  n: NodeInfo
  onAction: () => void
}

function NodeCard({ n, onAction }: NodeCardProps) {
  const toast = useToast()
  const [busy, setBusy] = useState('')
  const c = n.capacity
  const memUsed = c.mem_total_mb - c.mem_available_mb
  const memFrac = c.mem_total_mb ? memUsed / c.mem_total_mb : 0
  const loadFrac = c.cpu_cores ? c.load1 / c.cpu_cores : 0
  const overcommit = c.mem_total_mb ? c.mem_committed_mb / c.mem_total_mb : 0
  const cordoned = n.status === 'cordoned' || n.status === 'draining'
  const labels = n.labels ? Object.entries(n.labels) : []

  async function run(action: string, fn: () => Promise<unknown>) {
    setBusy(action)
    try { await fn() } catch (e) { toast.error(errText(e)) }
    finally { setBusy(''); onAction() }
  }

  return (
    <div className="panel" style={{ marginBottom: 14 }}>
      <div className="ph">
        <h2 className="fleet-node-head">
          <StatusPill status={n.status} />
          {n.id}
          <span className="fleet-node-cores"> · {c.cpu_cores} cores</span>
        </h2>
        <div className="rgt"><span style={{ fontSize: 12, color: 'var(--sub)' }}>{c.sandboxes_active} sandboxes</span></div>
      </div>

      <div className="fleet-node-chips">
        <span className="chip">zone · {n.zone || 'default'}</span>
        {n.runtime && <span className="chip">runtime · {n.runtime}</span>}
        {labels.map(([k, v]) => (
          <span key={k} className="chip">{k}={v}</span>
        ))}
      </div>

      <div className="fleet-node-meters">
        <div>
          <MeterBar label="CPU" fraction={c.cpu_percent / 100} value={`${c.cpu_percent.toFixed(0)}%`} />
          <MeterBar label={`Load (1m / ${c.cpu_cores} cores)`} fraction={loadFrac} value={c.load1.toFixed(1)} />
        </div>
        <div>
          <MeterBar label="RAM" fraction={memFrac} value={`${(memUsed / 1024).toFixed(1)} / ${(c.mem_total_mb / 1024).toFixed(0)} GB`} />
          <div style={{ fontSize: 12, color: 'var(--sub)', marginTop: 4 }}>
            disk free <b className="fleet-mono">{c.disk_free_gb.toFixed(0)} GB</b>
            {' · '}committed <b style={{ color: overcommit > 1 ? '#dc2626' : 'var(--ink)', fontFamily: 'var(--mono)' }}>
              {(c.mem_committed_mb / 1024).toFixed(1)} GB</b>
            {c.vcpu_committed != null && <span> · {c.vcpu_committed} vCPU committed</span>}
            {overcommit > 1 && <span className="fleet-over"> ({overcommit.toFixed(1)}× over-committed)</span>}
          </div>
        </div>
      </div>

      {n.url && (
        <div className="fleet-node-url">{n.url}</div>
      )}

      <div className="fleet-node-actions">
        {cordoned ? (
          <button className="btn" disabled={!!busy} onClick={() => run('uncordon', () => uncordonNode(n.id))}>
            {busy === 'uncordon' ? 'Uncordoning…' : 'Uncordon'}
          </button>
        ) : (
          <button className="btn" disabled={!!busy} onClick={() => run('cordon', () => cordonNode(n.id))}>
            {busy === 'cordon' ? 'Cordoning…' : 'Cordon'}
          </button>
        )}
        <button className="btn" disabled={!!busy || n.status === 'draining'} onClick={() => run('drain', () => drainNode(n.id))}>
          {busy === 'drain' ? 'Draining…' : 'Drain'}
        </button>
        {n.status === 'unreachable' && (
          <button
            className="btn fleet-over"
            disabled={!!busy}
            onClick={() => { if (confirm(`Forget node ${n.id} from the registry?`)) run('remove', () => removeNode(n.id)) }}
          >
            {busy === 'remove' ? 'Removing…' : 'Remove'}
          </button>
        )}
      </div>
    </div>
  )
}

interface StatProps {
  label: string
  value: string
}

function Stat({ label, value }: StatProps) {
  return (
    <div className="fleet-stat">
      <span className="fleet-stat-label">{label}</span>
      <span className="fleet-stat-value">{value}</span>
    </div>
  )
}

function SummaryStrip({ stats }: { stats: ClusterStats }) {
  const rec = stats.recommendation
  const rs = recStyle(rec.level)
  return (
    <div className="panel" style={{ marginBottom: 16 }}>
      <div className="fleet-summary-grid">
        <Stat label="Healthy" value={`${stats.nodes_healthy}/${stats.nodes_total}`} />
        <Stat label="Available" value={String(stats.nodes_available)} />
        <Stat label="Full" value={String(stats.nodes_full)} />
        <Stat label="Cores" value={String(stats.cores_total)} />
        <Stat label="Free RAM" value={`${stats.mem_free_gb} GB`} />
        <Stat label="Sandboxes" value={String(stats.sandboxes_active)} />
        <Stat label="Headroom" value={`${stats.headroom_sandboxes}`} />
      </div>
      <div className="fleet-rec" style={{ background: rs.bg, color: rs.fg, borderColor: rs.line }}>
        <b className="fleet-rec-level">{rec.level}</b>
        <span>{rec.message}</span>
        {rec.suggested_nodes > 0 && (
          <span className="fleet-rec-add">add {rec.suggested_nodes} node{rec.suggested_nodes !== 1 ? 's' : ''}</span>
        )}
      </div>
    </div>
  )
}

export default function Fleet() {
  const { data: nodesData, refetch } = usePoll<{ nodes: NodeInfo[] }>(listNodes, 2000, [])
  const { data: stats } = usePoll<ClusterStats>(getClusterStats, 2000, [])
  const nodes = nodesData?.nodes ?? []

  const zones = new Map<string, NodeInfo[]>()
  for (const n of nodes) {
    const z = n.zone || 'default'
    if (!zones.has(z)) zones.set(z, [])
    zones.get(z)!.push(n)
  }
  const zoneNames = Array.from(zones.keys()).sort()

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Fleet</h1>
          <p>
            {nodes.length} node{nodes.length !== 1 ? 's' : ''} across {zoneNames.length} zone{zoneNames.length !== 1 ? 's' : ''}
            {' '}· the cluster fleet · auto · 2s
          </p>
        </div>
      </div>

      {stats && <SummaryStrip stats={stats} />}

      {nodes.length === 0 ? (
        <div className="panel"><div className="empty">No nodes reporting yet.</div></div>
      ) : (
        zoneNames.map(zone => (
          <div key={zone} style={{ marginBottom: 6 }}>
            <div className="sect" style={{ marginTop: 4 }}>zone · {zone}</div>
            {zones.get(zone)!.map(n => <NodeCard key={n.id} n={n} onAction={refetch} />)}
          </div>
        ))
      )}
    </div>
  )
}

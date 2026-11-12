import { useNavigate } from 'react-router-dom'
import { usePoll } from '../hooks/useApi'
import { getClusterStats } from '../api'
import type { ClusterStats, ZoneStat } from '../types'
import { errText } from '../lib/errText'
import MeterBar from '../components/MeterBar'
import ObjectStoreCard from '../components/ObjectStoreCard'

function zoneStatusStyle(status: string): { bg: string; fg: string; line: string } {
  switch (status) {
    case 'full':
      return { bg: 'rgba(220,38,38,.12)', fg: '#dc2626', line: 'rgba(220,38,38,.28)' }
    case 'tight':
      return { bg: 'rgba(217,119,6,.13)', fg: '#d97706', line: 'rgba(217,119,6,.28)' }
    default:
      return { bg: 'var(--greensoft)', fg: 'var(--green)', line: 'var(--greenline)' }
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

function ZoneGauge({ z }: { z: ZoneStat }) {
  const s = zoneStatusStyle(z.status)
  const maxStd = z.headroom_sandboxes + z.sandboxes_active
  const usedFrac = maxStd ? z.sandboxes_active / maxStd : 0
  return (
    <div className="panel" style={{ marginBottom: 14 }}>
      <div className="ph">
        <h2 className="sys-zone-title">
          zone · {z.zone}
          <span style={{
            display: 'inline-flex', alignItems: 'center', gap: 6, borderRadius: 8, padding: '2px 9px',
            fontSize: 11.5, fontWeight: 600, background: s.bg, color: s.fg, border: `1px solid ${s.line}`,
          }}>
            {z.status}
          </span>
        </h2>
        <div className="rgt"><span style={{ fontSize: 12, color: 'var(--sub)' }}>{z.nodes} node{z.nodes !== 1 ? 's' : ''} · {z.cores} cores</span></div>
      </div>
      <div className="sys-zone-body">
        <MeterBar label="Capacity used" fraction={usedFrac} value={`${z.sandboxes_active} / ${maxStd} sandboxes`} />
        <div style={{ fontSize: 12, color: 'var(--sub)', marginTop: 4 }}>
          headroom <b style={{ color: 'var(--ink)', fontFamily: 'var(--mono)' }}>{z.headroom_sandboxes}</b> more sandboxes
          {' · '}free RAM <b style={{ color: 'var(--ink)', fontFamily: 'var(--mono)' }}>{z.mem_free_gb} GB</b>
        </div>
      </div>
    </div>
  )
}

interface StatProps { label: string; value: string }
function Stat({ label, value }: StatProps) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
      <span style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '.05em', color: 'var(--dim)', fontWeight: 600 }}>{label}</span>
      <span style={{ fontSize: 20, fontWeight: 700, fontFamily: 'var(--mono)', color: 'var(--ink)' }}>{value}</span>
    </div>
  )
}

export default function System() {
  const navigate = useNavigate()
  const { data: stats, error } = usePoll<ClusterStats>(getClusterStats, 2000, [])

  if (error && !stats) {
    return <div className="wrap"><div className="panel"><div className="empty">Failed to load cluster stats: {errText(error)}</div></div></div>
  }
  if (!stats) {
    return <div className="wrap"><div className="panel"><div className="empty">Loading…</div></div></div>
  }

  const rec = stats.recommendation
  const rs = recStyle(rec.level)

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">System / Scale</h1>
          <p>Per-zone capacity and the cluster scale recommendation · auto · 2s</p>
        </div>
      </div>

      <div className="panel sys-banner" style={{ borderColor: rs.line }}>
        <div className="sys-banner-body" style={{ background: rs.bg }}>
          <div className="sys-banner-txt">
            <div style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '.06em', fontWeight: 700, color: rs.fg }}>
              scale · {rec.level}
            </div>
            <div style={{ marginTop: 6, fontSize: 16, fontWeight: 600, color: 'var(--ink)' }}>{rec.message}</div>
          </div>
          {rec.suggested_nodes > 0 && (
            <button className="btn primary" onClick={() => navigate('/add-node')}>
              Add {rec.suggested_nodes} node{rec.suggested_nodes !== 1 ? 's' : ''} →
            </button>
          )}
        </div>
      </div>

      <div className="panel" style={{ marginBottom: 16 }}>
        <div className="ph"><h2>Cluster totals</h2><span className="meta">all zones</span></div>
        <div className="sys-stat-grid">
          <Stat label="Nodes" value={`${stats.nodes_healthy}/${stats.nodes_total}`} />
          <Stat label="Available" value={String(stats.nodes_available)} />
          <Stat label="Cores" value={String(stats.cores_total)} />
          <Stat label="RAM total" value={`${stats.mem_total_gb} GB`} />
          <Stat label="RAM free" value={`${stats.mem_free_gb} GB`} />
          <Stat label="Sandboxes" value={String(stats.sandboxes_active)} />
          <Stat label="Headroom" value={String(stats.headroom_sandboxes)} />
        </div>
      </div>

      <ObjectStoreCard />

      {stats.zones.length === 0 ? (
        <div className="panel"><div className="empty">No zones reporting yet.</div></div>
      ) : (
        stats.zones.map(z => <ZoneGauge key={z.zone} z={z} />)
      )}
    </div>
  )
}

import { layoutPlan, describeGraph, type GraphNode } from '../lib/graphLayout'
import type { Plan } from '../lib/plan'
import type { Verdicted } from '../lib/launchVerdict'

export type GraphPhase =
  | 'proposed'
  | 'live'
  | 'past'

function toneFor(v: Verdicted | null | undefined, phase: GraphPhase): { cls: string; label: string } {
  if (phase === 'proposed') return { cls: 'ag-proposed', label: 'proposed' }
  if (!v) {
    return phase === 'live'
      ? { cls: 'ag-starting', label: 'starting' }
      : { cls: 'ag-idle', label: 'not recorded' }
  }
  switch (v.verdict) {
    case 'serving':
    case 'no-preview':
    case 'service':
      return { cls: 'ag-ok', label: 'serving' }
    case 'app-error':
    case 'not-listening':
      return { cls: 'ag-bad', label: 'not listening' }
    case 'unconfirmed':
      return { cls: 'ag-idle', label: 'unconfirmed' }
  }
}

export default function ArchitectureView({
  plan,
  verdicts,
  phase = 'proposed',
  onSelect,
  selected,
}: {
  plan: Plan
  verdicts?: Record<string, Verdicted | null>
  phase?: GraphPhase
  onSelect?: (serviceId: string) => void
  selected?: string | null
}) {
  const g = layoutPlan(plan)
  if (g.nodes.length === 0) return <p className="build-eg">No services in this plan.</p>

  const at = (id: string) => g.nodes.find(n => n.id === id)

  return (
    <div className="ag-wrap">
      <svg
        className="ag-svg"
        viewBox={`0 0 ${g.width} ${g.height}`}
        role="img"
        aria-label={describeGraph(g)}
        preserveAspectRatio="xMinYMin meet"
      >
        <defs>
          <marker id="ag-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto">
            <path d="M0 0 L8 4 L0 8 z" fill="var(--sub)" />
          </marker>
        </defs>

        {g.edges.map(e => {
          const a = at(e.from)
          const b = at(e.to)
          if (!a || !b) return null
          const x1 = a.x + a.w
          const y1 = a.y + a.h / 2
          const x2 = b.x
          const y2 = b.y + b.h / 2
          const mid = (x1 + x2) / 2
          return (
            <g key={`${e.from}-${e.to}-${e.label}`}>
              <path
                d={`M${x1} ${y1} H${mid} V${y2} H${x2}`}
                className="ag-edge"
                markerEnd="url(#ag-arrow)"
              />
              <text x={mid} y={Math.min(y1, y2) - 6} className="ag-edge-l" textAnchor="middle">
                {e.label}
              </text>
            </g>
          )
        })}

        {g.nodes.map((n: GraphNode) => {
          const tone = toneFor(verdicts?.[n.id], phase)
          const isSel = selected === n.id
          return (
            <g
              key={n.id}
              className={`ag-node ${tone.cls}${isSel ? ' ag-sel' : ''}${onSelect ? ' ag-click' : ''}`}
              transform={`translate(${n.x},${n.y})`}
              tabIndex={onSelect ? 0 : -1}
              role={onSelect ? 'button' : undefined}
              aria-label={`${n.label}, ${tone.label}. ${n.reason}`}
              onClick={() => onSelect?.(n.id)}
              onKeyDown={ev => {
                if (ev.key === 'Enter' || ev.key === ' ') {
                  ev.preventDefault()
                  onSelect?.(n.id)
                }
              }}
            >
              <rect className="ag-box" width={n.w} height={n.h} rx="8" />
              <text x="11" y="20" className="ag-id">{n.id}</text>
              <text x="11" y="37" className="ag-name">{n.label}</text>
              <text x="11" y="52" className="ag-meta">
                {n.port ? `:${n.port} · ` : ''}{tone.label}
              </text>
              <circle className="ag-dot" cx={n.w - 14} cy="18" r="4" />
            </g>
          )
        })}
      </svg>

      {phase === 'proposed' && (
        <p className="build-eg ag-note">
          Nothing is running yet — this is the plan, not its status.
        </p>
      )}
    </div>
  )
}

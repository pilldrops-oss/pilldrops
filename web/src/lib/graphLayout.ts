import type { Plan, ServiceRole } from './plan'

// Pure Plan-to-coordinates layout, kept apart from rendering so it is testable and
// deterministic. Consumers sit left, their dependencies right, so edges read left-to-right.

const NODE_W = 150
const NODE_H = 62
const GAP_X = 96
const GAP_Y = 18
const PAD = 14

export interface GraphNode {
  id: string
  label: string
  role: ServiceRole
  port?: number
  reason: string
  x: number
  y: number
  w: number
  h: number
}

export interface GraphEdge {
  from: string
  to: string
  /** The env variable this link injects. */
  label: string
  reason: string
}

export interface Graph {
  nodes: GraphNode[]
  edges: GraphEdge[]
  width: number
  height: number
}

// Column index: 0 for consumers, 1 for their targets.
function depthOf(id: string, plan: Plan): number {
  return plan.links.some(l => l.to === id) ? 1 : 0
}

export function layoutPlan(plan: Plan): Graph {
  const columns = new Map<number, typeof plan.services>()
  for (const s of plan.services) {
    const d = depthOf(s.id, plan)
    columns.set(d, [...(columns.get(d) ?? []), s])
  }

  const nodes: GraphNode[] = []
  for (const [depth, services] of [...columns.entries()].sort((a, b) => a[0] - b[0])) {
    services.forEach((s, i) => {
      nodes.push({
        id: s.id,
        label: s.template.name,
        role: s.role,
        port: s.template.default_port,
        reason: s.reason,
        x: PAD + depth * (NODE_W + GAP_X),
        y: PAD + i * (NODE_H + GAP_Y),
        w: NODE_W,
        h: NODE_H,
      })
    })
  }

  const edges: GraphEdge[] = plan.links.map(l => ({
    from: l.from,
    to: l.to,
    label: l.env,
    reason: l.reason,
  }))

  // Minimum of one node's space, so an empty graph does not collapse the container.
  const width = Math.max(...nodes.map(n => n.x + n.w), NODE_W) + PAD
  const height = Math.max(...nodes.map(n => n.y + n.h), NODE_H) + PAD

  return { nodes, edges, width, height }
}

/** describeGraph renders the graph in words, for a screen reader. */
export function describeGraph(g: Graph): string {
  if (g.nodes.length === 0) return 'No services in this plan.'
  const lines = g.nodes.map(n => {
    const to = g.edges.filter(e => e.from === n.id)
    const wiring = to.length
      ? ` Connects to ${to.map(e => `${e.to} via ${e.label}`).join(', ')}.`
      : ''
    return `${n.id}: ${n.label}${n.port ? ` on port ${n.port}` : ''}. ${n.reason}.${wiring}`
  })
  return lines.join(' ')
}

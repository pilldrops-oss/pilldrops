import { useCallback, useEffect, useRef, useState } from 'react'
import { agentEventsURL } from '../api'

// Separate from useSSE: this is many event types over a durable journal with a resume cursor.

/** One journal event; unknown types are kept. */
export type AgentEvent = {
  seq: number
  at: string
  type: string
  turn_id?: string
  step?: number
  payload?: Record<string, unknown>
}

/** A tool call assembled from its start and end events. */
export type ToolStep = {
  id: string
  seq: number
  tool: string
  detail?: string
  state: 'running' | 'done' | 'failed'
  ms?: number
  result?: string
}

export type AgentStreamState = {
  events: AgentEvent[]
  text: string
  thinking: string
  tools: ToolStep[]
  /** The highest seq seen; the resume cursor. */
  cursor: number
  connected: boolean
  /** True once the agent's output hit its per-turn ceiling and the middle was dropped. */
  truncated: boolean
  /** Set when the stream degraded to polling because the tab lost focus. */
  polling: boolean
  /** What the last finished turn reported; fields the agent did not report are absent, never zero. */
  usage?: TurnUsage
}

export type TurnUsage = {
  inputTokens?: number
  outputTokens?: number
  cacheReadTokens?: number
  costUSD?: number
}

const MAX_EVENTS = 2000

const EVENT_TYPES = [
  'turn.queued', 'turn.started', 'turn.output', 'turn.thinking',
  'turn.output_truncated', 'turn.finished', 'turn.failed', 'turn.interrupted',
  'tool.started', 'tool.finished', 'tool.failed',
  'session.starting', 'session.ready', 'session.degraded', 'session.stopped',
  'session.config_changed', 'spend.threshold',
  'approval.requested', 'approval.granted', 'approval.denied',
]

// A hidden tab downgrades to polling after this long, so background dashboards do not
// hold live sockets into sandboxes.
const BLUR_GRACE_MS = 60_000

// While the stream is live it already carries every transition, so HTTP polling is only
// a reconciliation safety net; once degraded, HTTP is the only transport left.
export const POLL_DEGRADED_MS = 10_000
export const POLL_LIVE_MS = 30_000

export function pollIntervalMs(polling: boolean): number {
  return polling ? POLL_DEGRADED_MS : POLL_LIVE_MS
}

const RECONNECT_BASE_MS = 1_000
const RECONNECT_MAX_MS = 30_000

// backoffMs returns the delay before reconnect attempt n (0-based). Full jitter, because
// when a node restarts every watching dashboard fails at once and would retry in lockstep.
export function backoffMs(attempt: number, rnd: () => number = Math.random): number {
  const ceiling = Math.min(RECONNECT_MAX_MS, RECONNECT_BASE_MS * 2 ** Math.min(attempt, 10))
  // Never below the base: a zero-delay retry against a down node is a tight loop.
  return RECONNECT_BASE_MS + rnd() * (ceiling - RECONNECT_BASE_MS)
}

/** useAgentStream follows one sandbox's agent journal; `enabled` is false for non-agent sandboxes. */
export function useAgentStream(sandboxID: string, enabled: boolean, startSeq = 0) {
  const [state, setState] = useState<AgentStreamState>({
    events: [], text: '', thinking: '', tools: [],
    cursor: startSeq, connected: false, truncated: false, polling: false,
  })

  // A ref so the reconnect effect reads the latest cursor without re-subscribing per event.
  const cursor = useRef(startSeq)
  const es = useRef<EventSource | null>(null)
  // Reset on a successful open, not per event: a stream that connects and dies is still failing.
  const attempts = useRef(0)
  const retryTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const [hidden, setHidden] = useState(() =>
    typeof document !== 'undefined' ? document.hidden : false)

  const apply = useCallback((ev: AgentEvent) => {
    if (ev.seq <= cursor.current) return // replay overlap
    cursor.current = ev.seq

    setState(prev => {
      const next: AgentStreamState = {
        ...prev,
        cursor: ev.seq,
        events: [...prev.events, ev].slice(-MAX_EVENTS),
      }
      const p = ev.payload ?? {}
      const str = (k: string) => (typeof p[k] === 'string' ? (p[k] as string) : '')

      switch (ev.type) {
        case 'turn.output':
          next.text = prev.text + str('text')
          break
        case 'turn.thinking':
          next.thinking = prev.thinking + str('text')
          break
        case 'turn.output_truncated':
          next.truncated = true
          break
        case 'turn.finished': {
          // A missing token count must stay undefined, not become zero.
          const num = (k: string) => (typeof p[k] === 'number' ? (p[k] as number) : undefined)
          const u: TurnUsage = {
            inputTokens: num('input_tokens'),
            outputTokens: num('output_tokens'),
            cacheReadTokens: num('cache_read_tokens'),
            costUSD: num('model_cost_usd'),
          }
          // Do not replace a populated usage with an all-empty one.
          if (u.inputTokens || u.outputTokens || u.cacheReadTokens || u.costUSD) {
            next.usage = u
          }
          break
        }
        case 'tool.started':
          next.tools = [...prev.tools, {
            id: str('tool_id') || `${ev.seq}`,
            seq: ev.seq, tool: str('tool'), detail: str('detail'), state: 'running',
          }]
          break
        case 'tool.finished':
        case 'tool.failed': {
          const id = str('tool_id')
          const done = ev.type === 'tool.finished' ? 'done' : 'failed'
          const ms = typeof p.ms === 'number' ? (p.ms as number) : undefined
          // Match by id: agents interleave calls, so "most recent running" misattributes outcomes.
          let matched = false
          next.tools = prev.tools.map(t => {
            if (matched || t.id !== id || t.state !== 'running') return t
            matched = true
            return { ...t, state: done as ToolStep['state'], ms, result: str('result') }
          })
          if (!matched) {
            // An end with no start: a reconnect resumed past the start event. Show it anyway.
            next.tools = [...next.tools, {
              id: id || `${ev.seq}`, seq: ev.seq, tool: str('tool'),
              detail: str('detail'), state: done as ToolStep['state'], ms,
              result: str('result'),
            }]
          }
          break
        }
      }
      return next
    })
  }, [])

  // Reset when the sandbox changes so one transcript does not bleed into another.
  useEffect(() => {
    cursor.current = startSeq
    setState({
      events: [], text: '', thinking: '', tools: [],
      cursor: startSeq, connected: false, truncated: false, polling: false,
    })
  }, [sandboxID, startSeq])

  useEffect(() => {
    if (typeof document === 'undefined') return
    const onVis = () => setHidden(document.hidden)
    document.addEventListener('visibilitychange', onVis)
    return () => document.removeEventListener('visibilitychange', onVis)
  }, [])

  // Delayed rather than immediate so flipping between tabs does not thrash the connection.
  const [degraded, setDegraded] = useState(false)
  useEffect(() => {
    if (!hidden) {
      setDegraded(false)
      return
    }
    const t = setTimeout(() => setDegraded(true), BLUR_GRACE_MS)
    return () => clearTimeout(t)
  }, [hidden])

  useEffect(() => {
    if (!enabled || !sandboxID) return
    if (degraded) {
      setState(s => ({ ...s, connected: false, polling: true }))
      return
    }
    if (typeof EventSource === 'undefined') {
      // Report polling rather than throw; there is no error boundary above this.
      setState(s => ({ ...s, connected: false, polling: true }))
      return
    }
    setState(s => ({ ...s, polling: false }))

    // Guards against a reconnect scheduled by a connection this effect has since torn down.
    let live = true

    const open = () => {
      if (!live) return
      const src = new EventSource(agentEventsURL(sandboxID, cursor.current))
      es.current = src
      wire(src)
    }

    const scheduleReconnect = () => {
      if (!live) return
      es.current?.close()
      es.current = null
      const delay = backoffMs(attempts.current)
      attempts.current += 1
      retryTimer.current = setTimeout(open, delay)
    }

    const src = new EventSource(agentEventsURL(sandboxID, cursor.current))
    es.current = src

    function wire(target: EventSource) {
      target.onmessage = onEvent
      for (const t of EVENT_TYPES) target.addEventListener(t, onEvent as EventListener)
      target.onopen = () => {
        attempts.current = 0
        setState(s => ({ ...s, connected: true }))
      }
      target.onerror = () => {
        setState(s => ({ ...s, connected: false }))
        // EventSource's own retry is a fixed schedule; backoffMs exists to break that lockstep.
        scheduleReconnect()
      }
    }

    const onEvent = (e: MessageEvent) => {
      try {
        apply(JSON.parse(e.data) as AgentEvent)
      } catch {
        // One unparseable frame is not a broken stream.
      }
    }
    // Unnamed frames and every known type land on the same handler so unknown types still arrive.
    wire(src)

    return () => {
      live = false
      if (retryTimer.current) clearTimeout(retryTimer.current)
      retryTimer.current = null
      src.close()
      es.current?.close()
      es.current = null
    }
  }, [sandboxID, enabled, degraded, apply])

  return state
}

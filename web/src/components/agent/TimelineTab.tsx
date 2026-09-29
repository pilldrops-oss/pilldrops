import { useCallback, useEffect, useState } from 'react'
import { listAgentTurns, type AgentTurn } from '../../api'

const POLL_MS = 5000

export function TimelineTab({ sandboxID }: { sandboxID: string }) {
  const [turns, setTurns] = useState<AgentTurn[] | null>(null)
  const [source, setSource] = useState<'node' | 'mirror' | undefined>(undefined)
  const [note, setNote] = useState<string | undefined>(undefined)
  const [err, setErr] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const r = await listAgentTurns(sandboxID)
      setTurns(r.turns ?? [])
      setSource(r.served_from)
      setNote(r.note)
      setErr(null)
    } catch (e) {
      setErr((e as Error).message)
    }
  }, [sandboxID])

  useEffect(() => {
    void refresh()
    const iv = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(iv)
  }, [refresh])

  if (!turns) {
    if (err) {
      return (
        <section className="ws-card">
          <h3>Timeline</h3>
          <p className="ws-muted">Could not read this workstation's history: {err}</p>
        </section>
      )
    }
    return (
      <section className="ws-card">
        <h3>Timeline</h3>
        <ul className="agent-timeline">
          {[0, 1, 2].map((i) => (
            <li key={i} className="agent-timeline-skeleton"><span /><span /></li>
          ))}
        </ul>
      </section>
    )
  }

  return (
    <section className="ws-card">
      <h3>Timeline</h3>

      {source === 'mirror' && (
        <p className="ws-warn" role="status">
          {note ?? 'This workstation is not running; these turns come from the control ' +
            'plane’s copy.'}
        </p>
      )}
      {err && (
        <p className="ws-muted" role="status">
          Showing the last successful read — the latest poll failed ({err}).
        </p>
      )}

      {turns.length === 0 ? (
        <p className="ws-muted">
          Nothing yet. Turns appear here however they were started — from this dashboard, the
          API, a chat bridge, or a schedule.
        </p>
      ) : (
        <ul className="agent-timeline">
          {turns.map((t) => <TurnRow key={t.id} turn={t} />)}
        </ul>
      )}
    </section>
  )
}

function TurnRow({ turn }: { turn: AgentTurn }) {
  const [open, setOpen] = useState(false)
  return (
    <li className={`agent-timeline-row agent-state-${turn.state}`}>
      <button className="agent-timeline-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="agent-timeline-when">{when(turn)}</span>
        <span className="agent-timeline-req">{turn.request}</span>
        <span className={`agent-chip agent-chip-${turn.state}`}>{stateLabel(turn.state)}</span>
      </button>

      <div className="agent-timeline-meta">
        <span title="Where this turn came from">{sourceLabel(turn.source)}</span>
        {turn.mode && <span title="Autonomy for this turn">{turn.mode}</span>}
        <span>{duration(turn)}</span>
        <span>{cost(turn)}</span>
      </div>

      {open && (
        <div className="agent-timeline-body">
          {turn.error && <p className="ws-err">{turn.error}</p>}
          {turn.response
            ? <pre>{turn.response}</pre>
            : <p className="ws-muted">{noResponseReason(turn)}</p>}
        </div>
      )}
    </li>
  )
}

function when(t: AgentTurn): string {
  const at = t.started_at ?? t.queued_at
  if (!at) return ''
  return new Date(at).toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  })
}

function stateLabel(state: AgentTurn['state']): string {
  if (state === 'awaiting_approval') return 'waiting for you'
  return state
}

function sourceLabel(source?: string): string {
  if (!source) return 'unknown source'
  if (source.startsWith('token:')) {
    return 'API token'
  }
  switch (source) {
    case 'chat': return 'chat'
    case 'telegram': return 'Telegram'
    case 'discord': return 'Discord'
    case 'cron': return 'schedule'
    case 'dashboard': return 'dashboard'
    case 'api': return 'API'
    default: return source
  }
}

function duration(t: AgentTurn): string {
  if (!t.started_at) return t.state === 'queued' ? 'not started' : ''
  const end = t.ended_at ? new Date(t.ended_at) : new Date()
  const ms = end.getTime() - new Date(t.started_at).getTime()
  if (ms < 0) return ''
  const s = Math.round(ms / 1000)
  const text = s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`
  return t.ended_at ? text : `${text} so far`
}

function cost(t: AgentTurn): string {
  if (typeof t.cost_usd !== 'number') return 'cost not reported'
  return `$${t.cost_usd.toFixed(4)}`
}

function noResponseReason(t: AgentTurn): string {
  switch (t.state) {
    case 'queued': return 'Queued — it has not started yet.'
    case 'running': return 'Still running.'
    case 'awaiting_approval':
      return 'Paused: it needs your approval before it can carry on. The Agent tab has the decision.'
    case 'interrupted': return 'Interrupted before it produced an answer.'
    case 'failed': return 'Failed without producing an answer.'
    default:
      return 'This turn finished without recording an answer.'
  }
}

import { useEffect, useState } from 'react'
import { getAgentIsolation, type IsolationFact } from '../../api'

export function IsolationCard({ sandboxID }: { sandboxID: string }) {
  const [facts, setFacts] = useState<IsolationFact[] | null>(null)
  const [err, setErr] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    getAgentIsolation(sandboxID)
      .then((r) => { if (live) setFacts(r.facts ?? []) })
      .catch((e) => { if (live) setErr((e as Error).message) })
    return () => { live = false }
  }, [sandboxID])

  if (err) {
    return (
      <section className="ws-card agent-isolation">
        <h3>Isolation</h3>
        <p className="ws-muted">Could not check this workstation's isolation: {err}</p>
      </section>
    )
  }
  if (!facts) {
    return (
      <section className="ws-card agent-isolation">
        <h3>Isolation</h3>
        <ul className="agent-isolation-list">
          {[0, 1, 2].map((i) => (
            <li key={i} className="agent-isolation-skeleton"><span /><span /></li>
          ))}
        </ul>
      </section>
    )
  }

  return (
    <section className="ws-card agent-isolation">
      <h3>Isolation</h3>
      <ul className="agent-isolation-list">
        {facts.map((f) => (
          <li key={f.label} className={`agent-isolation-${f.state}`}>
            <span className="agent-isolation-mark" aria-hidden>{mark(f.state)}</span>
            <div>
              <span className="agent-isolation-label">{f.label}</span>
              <span className="agent-isolation-detail">{f.detail}</span>
            </div>
            <span className="ws-sr-only">{stateWord(f.state)}</span>
          </li>
        ))}
      </ul>
    </section>
  )
}

function mark(state: IsolationFact['state']): string {
  if (state === 'yes') return '✓'
  if (state === 'no') return '✗'
  return '?'
}

function stateWord(state: IsolationFact['state']): string {
  if (state === 'yes') return 'yes'
  if (state === 'no') return 'no'
  return 'could not be verified'
}

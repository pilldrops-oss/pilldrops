import { useCallback, useEffect, useState } from 'react'
import {
  getAgentStatus, interruptAgentTurn, listAgentTurns, submitAgentTurn,
  dequeueAgentTurn, type AgentStatus, type AgentTurn,
} from '../../api'
import { useAgentStream, pollIntervalMs, type ToolStep } from '../../hooks/useAgentStream'
import { ApprovalQueue } from './ApprovalCard'

export function AgentPanel({ sandboxID }: { sandboxID: string }) {
  const [status, setStatus] = useState<AgentStatus | null>(null)
  const [turns, setTurns] = useState<AgentTurn[]>([])
  const [draft, setDraft] = useState('')
  const [mode, setMode] = useState<string | undefined>(undefined)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const [s, t] = await Promise.all([getAgentStatus(sandboxID), listAgentTurns(sandboxID)])
      setStatus(s)
      setTurns(t.turns ?? [])
      setErr(null)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setLoaded(true)
    }
  }, [sandboxID])

  const session = status?.session
  const running = session?.current_turn
  const stream = useAgentStream(sandboxID, true, running?.seq_start ? running.seq_start - 1 : 0)

  useEffect(() => {
    void refresh()
    const iv = setInterval(() => void refresh(), pollIntervalMs(stream.polling))
    return () => clearInterval(iv)
  }, [refresh, stream.polling])

  const send = async () => {
    const msg = draft.trim()
    if (!msg || busy) return
    setBusy(true)
    setErr(null)
    try {
      await submitAgentTurn(sandboxID, msg, mode ? { mode } : {})
      setDraft('')
      await refresh()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const stop = async () => {
    if (!running) return
    try {
      await interruptAgentTurn(sandboxID, running.id)
      await refresh()
    } catch (e) {
      setErr((e as Error).message)
    }
  }

  if (!loaded) {
    return (
      <div className="agent-skeleton" aria-busy="true" aria-label="Loading the agent">
        <span className="agent-skeleton-head" />
        <span className="agent-skeleton-body" />
        <span className="agent-skeleton-input" />
      </div>
    )
  }

  if (err && !status) {
    return (
      <div className="ws-panel">
        <p className="ws-error">The agent supervisor is not answering.</p>
        <p className="ws-muted">{err}</p>
        <p className="ws-muted">
          It may still be starting. If this persists the workstation may be paused —
          sending a task will wake it.
        </p>
      </div>
    )
  }

  return (
    <div className="ws-panel agent-panel">
      <AgentHeader status={status} connected={stream.connected} polling={stream.polling}
        onStop={running ? stop : undefined} />

      {session?.state === 'degraded' && session.degraded_reason && (
        <div className="ws-warn" role="status">{session.degraded_reason}</div>
      )}

      {status?.agent.remembers === false && status.agent.memory_note && (
        <div className="ws-warn agent-memory-note" role="status">{status.agent.memory_note}</div>
      )}

      {session?.spend?.exceeded && (
        <div className="ws-warn" role="status">
          The spend cap has been reached, so queued tasks are paused. Raise or clear
          the cap to continue.
        </div>
      )}

      <ApprovalQueue sandboxID={sandboxID} onDecided={refresh} />

      <Transcript turns={turns} stream={stream} mode={session?.mode} agent={status?.agent.name} />

      <QueuePanel sandboxID={sandboxID} turns={turns} onChange={refresh} />

      {err && <div className="ws-error" role="alert">{err}</div>}

      <ModePicker
        autonomy={session?.autonomy}
        value={mode}
        onChange={setMode}
      />

      <div className="agent-composer">
        <textarea
          value={draft}
          onChange={e => setDraft(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              void send()
            }
          }}
          placeholder="Give the agent a task…"
          rows={3}
          disabled={busy}
          aria-label="Task for the agent"
        />
        <div className="agent-composer-row">
          <button className="ws-btn" onClick={() => void send()} disabled={busy || !draft.trim()}>
            {busy ? 'Sending…' : 'Send'}
          </button>
          <span className="ws-muted">Enter sends · Shift+Enter for a new line</span>
        </div>
      </div>
      <SpendNote spend={session?.spend} />
    </div>
  )
}

export function shortVersion(v: string): string {
  const i = v.indexOf(' (')
  return i > 0 ? v.slice(0, i) : v
}

// The tooltip and the visible label are one decision about the stream's state.
function linkState(polling: boolean, connected: boolean): { title: string; label: string } {
  if (polling) {
    return {
      title: 'This tab is in the background, so live updates paused to save resources',
      label: 'paused (background tab)',
    }
  }
  if (connected) return { title: 'Live', label: 'live' }
  return { title: 'Reconnecting…', label: 'reconnecting…' }
}

function AgentHeader({ status, connected, polling, onStop }: {
  status: AgentStatus | null
  connected: boolean
  polling: boolean
  onStop?: () => void
}) {
  const s = status?.session
  const link = linkState(polling, connected)
  return (
    <div className="agent-header">
      <span className={`agent-dot agent-${s?.state ?? 'starting'}`} aria-hidden />
      <strong>{status?.agent.name ?? 'Agent'}</strong>
      <span className="ws-muted">{s?.state ?? 'starting'}</span>
      {s?.agent_version && (
        <span className="ws-muted" title={s.agent_version}>v{shortVersion(s.agent_version)}</span>
      )}
      {s?.queue_depth ? <span className="ws-muted">{s.queue_depth} queued</span> : null}
      <span className="ws-muted" title={link.title}>{link.label}</span>
      {onStop && <button className="ws-danger" onClick={onStop}>Interrupt</button>}
    </div>
  )
}

function Transcript({ turns, stream, mode, agent }: {
  turns: AgentTurn[]
  stream: ReturnType<typeof useAgentStream>
  mode?: 'rich' | 'basic'
  agent?: string
}) {
  const history = [...turns].reverse()

  return (
    <div className="agent-transcript">
      {history.length === 0 && !stream.text && (
        <p className="ws-muted">
          No tasks yet. Describe what you want {agent ?? 'the agent'} to do and it will
          start working — you can close this tab, the task keeps running.
        </p>
      )}

      {history.map(t => (
        <div key={t.id} className="agent-turn">
          <div className="agent-msg agent-msg-user">{t.request}</div>
          {t.response && <div className="agent-msg agent-msg-agent">{t.response}</div>}
          <TurnMeta turn={t} />
        </div>
      ))}

      {mode === 'basic' && (
        <p className="ws-muted agent-mode-note">
          This agent reports plain output only, so per-step tool detail is not
          available for it. Its replies and exit status are complete.
        </p>
      )}

      {mode === 'rich' && stream.tools.length > 0 && (
        <ToolTimeline tools={stream.tools} />
      )}

      {stream.thinking && (
        <details className="agent-thinking">
          <summary>Reasoning</summary>
          <pre>{stream.thinking}</pre>
        </details>
      )}

      {stream.text && <pre className="agent-live">{stream.text}</pre>}

      <UsageChips usage={stream.usage} />
      {stream.truncated && (
        <p className="ws-warn agent-truncated" role="status">
          This turn produced more output than is kept, so the middle was dropped. The
          agent's result above is unaffected.
        </p>
      )}
    </div>
  )
}

function UsageChips({ usage }: { usage?: { inputTokens?: number; outputTokens?: number; cacheReadTokens?: number; costUSD?: number } }) {
  if (!usage) return null
  const chips: string[] = []
  if (usage.inputTokens) chips.push(`${fmtTokens(usage.inputTokens)} in`)
  if (usage.outputTokens) chips.push(`${fmtTokens(usage.outputTokens)} out`)
  if (usage.cacheReadTokens) chips.push(`${fmtTokens(usage.cacheReadTokens)} cached`)
  if (typeof usage.costUSD === 'number') chips.push(`$${usage.costUSD.toFixed(4)}`)
  if (chips.length === 0) return null
  return (
    <div className="agent-usage" title="Reported by the agent for its last finished turn">
      {chips.map(c => <span key={c} className="agent-chip">{c}</span>)}
    </div>
  )
}

function fmtTokens(n: number): string {
  if (n < 1000) return String(n)
  return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
}

function ToolTimeline({ tools }: { tools: ToolStep[] }) {
  return (
    <ol className="agent-tools">
      {tools.map(t => (
        <li key={`${t.id}-${t.seq}`} className={`agent-tool agent-tool-${t.state}`}>
          <span className="agent-tool-name">{t.tool || 'tool'}</span>
          {t.detail && <code className="agent-tool-detail">{t.detail}</code>}
          {typeof t.ms === 'number' && <span className="ws-muted">{t.ms}ms</span>}
          {t.state === 'failed' && t.result && (
            <span className="ws-error agent-tool-error">{t.result}</span>
          )}
        </li>
      ))}
    </ol>
  )
}

function TurnMeta({ turn }: { turn: AgentTurn }) {
  const bits: string[] = [turn.state]
  if (turn.mode) bits.push(turn.mode)
  if (turn.source) bits.push(`via ${turn.source}`)
  if (typeof turn.exit_code === 'number' && turn.exit_code !== 0) {
    bits.push(`exit ${turn.exit_code}`)
  }
  return (
    <div className="agent-turn-meta ws-muted">
      {bits.join(' · ')}
      {turn.error && <span className="ws-error"> — {turn.error}</span>}
    </div>
  )
}

function QueuePanel({ sandboxID, turns, onChange }: {
  sandboxID: string
  turns: AgentTurn[]
  onChange: () => void | Promise<void>
}) {
  const queued = turns.filter(t => t.state === 'queued')
  if (queued.length === 0) return null

  const remove = async (id: string) => {
    try {
      await dequeueAgentTurn(sandboxID, id)
      await onChange()
    } catch {
      await onChange()
    }
  }

  return (
    <div className="agent-queue">
      <span className="ws-muted">Queued ({queued.length}) — one task runs at a time</span>
      <ul>
        {queued.map(t => (
          <li key={t.id}>
            <span>{t.request}</span>
            <button onClick={() => void remove(t.id)} aria-label={`Remove ${t.request}`}>
              Remove
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}

function SpendNote({ spend }: { spend?: AgentStatus['session']['spend'] }) {
  if (!spend || spend.cap_usd <= 0) return null
  return (
    <p className="ws-muted agent-spend">
      ${spend.spent_usd.toFixed(2)} of ${spend.cap_usd.toFixed(2)} reported model spend
      {spend.turns_unreported > 0 && (
        <> · {spend.turns_unreported} turn{spend.turns_unreported === 1 ? '' : 's'} reported
          no cost, so this is a partial figure</>
      )}
    </p>
  )
}

function ModePicker({ autonomy, value, onChange }: {
  autonomy?: { available: string[]; default: string; guided_unavailable_reason?: string }
  value?: string
  onChange: (mode: string | undefined) => void
}) {
  if (!autonomy?.available?.length) return null
  const label: Record<string, string> = {
    supervised: 'Supervised — plan only, run nothing',
    guided: 'Guided — ask me before each action',
    autonomous: 'Autonomous — run without asking',
  }
  return (
    <div className="agent-mode">
      <label htmlFor="agent-mode-select">How much should it do on its own?</label>
      <select
        id="agent-mode-select"
        value={value ?? ''}
        onChange={e => onChange(e.target.value || undefined)}
      >
        <option value="">
          Default ({label[autonomy.default]?.split(' — ')[0] ?? autonomy.default})
        </option>
        {autonomy.available.map(m => (
          <option key={m} value={m}>{label[m] ?? m}</option>
        ))}
      </select>
      {autonomy.guided_unavailable_reason && (
        <p className="ws-muted agent-mode-note">
          Guided mode is not available here: {autonomy.guided_unavailable_reason}
        </p>
      )}
    </div>
  )
}

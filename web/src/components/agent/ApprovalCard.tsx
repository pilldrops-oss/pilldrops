import { useCallback, useEffect, useRef, useState } from 'react'
import {
  listAgentApprovals, decideAgentApproval,
  type AgentApproval, type AgentApprovalsResponse,
} from '../../api'

const POLL_MS = 2000

export function ApprovalQueue({ sandboxID, onDecided }: {
  sandboxID: string
  onDecided?: () => void
}) {
  const [data, setData] = useState<AgentApprovalsResponse | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [unavailable, setUnavailable] = useState<string | null>(null)
  const [busy, setBusy] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const d = await listAgentApprovals(sandboxID)
      setData(d)
      setErr(null)
      setUnavailable(null)
    } catch (e) {
      const msg = (e as Error).message
      if (/not available|gate_unavailable|no approval gate/i.test(msg)) {
        setUnavailable(msg)
        setData(null)
      } else {
        setErr(msg)
      }
    }
  }, [sandboxID])

  useEffect(() => {
    void refresh()
    const iv = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(iv)
  }, [refresh])

  const decide = async (
    a: AgentApproval,
    decision: 'allow' | 'deny',
    remember: 'once' | 'session' | 'sandbox',
  ) => {
    setBusy(a.id)
    try {
      await decideAgentApproval(sandboxID, a.id, decision, remember)
      await refresh()
      onDecided?.()
    } catch (e) {
      const msg = (e as Error).message
      setErr(/no longer pending|not pending|approval_not_pending|409/i.test(msg)
        ? 'That request timed out before your answer arrived, so it was denied. ' +
          'The agent can be asked to try again.'
        : msg)
    } finally {
      setBusy(null)
    }
  }

  if (unavailable) return null
  const pending = data?.approvals ?? []
  if (!pending.length) {
    return err ? <div className="ws-error" role="alert">{err}</div> : null
  }

  const readOnly = data?.can_decide === false

  return (
    <section className="agent-approvals" aria-label="Actions waiting for approval">
      <header className="agent-approvals-head">
        <strong>
          {pending.length === 1
            ? 'The agent is waiting for approval'
            : `${pending.length} actions are waiting for approval`}
        </strong>
        <span className="ws-muted">
          Anything not answered in time is refused, not allowed.
        </span>
      </header>

      {readOnly && (
        <div className="ws-warn" role="status">
          {data?.cannot_decide_reason ||
            'This session cannot answer approvals — it can only watch.'}
        </div>
      )}
      {err && <div className="ws-error" role="alert">{err}</div>}

      {pending.map(a => (
        <ApprovalRow
          key={a.id}
          approval={a}
          timeoutSeconds={data?.timeout_seconds ?? 0}
          busy={busy === a.id}
          readOnly={readOnly}
          onDecide={(d, r) => void decide(a, d, r)}
        />
      ))}
    </section>
  )
}

function allowLabel(busy: boolean, remember: string): string {
  if (busy) return 'Sending…'
  return remember === 'once' ? 'Allow once' : 'Allow'
}

function ApprovalRow({ approval, timeoutSeconds, busy, readOnly, onDecide }: {
  approval: AgentApproval
  timeoutSeconds: number
  busy: boolean
  readOnly: boolean
  onDecide: (decision: 'allow' | 'deny', remember: 'once' | 'session' | 'sandbox') => void
}) {
  const [showPayload, setShowPayload] = useState(false)
  const [remember, setRemember] = useState<'once' | 'session' | 'sandbox'>('once')
  const left = useCountdown(approval.requested_at, timeoutSeconds)

  return (
    <article className={`agent-approval${left !== null && left <= 60 ? ' agent-approval-urgent' : ''}`}>
      <div className="agent-approval-what">
        <code className="agent-approval-tool">{approval.tool}</code>
        <span className="agent-approval-target">{targetOf(approval)}</span>
      </div>

      {left !== null && (
        <div className="agent-approval-clock" role="timer">
          {left > 0
            ? `Refused in ${formatLeft(left)} if nobody answers`
            : 'Time is up — this is being refused'}
        </div>
      )}

      <button className="ws-link agent-approval-toggle"
        aria-expanded={showPayload}
        onClick={() => setShowPayload(v => !v)}>
        {showPayload ? 'Hide the details' : 'Show what it sent'}
      </button>
      {showPayload && (
        <>
          <pre className="agent-approval-payload">{approval.args_preview}</pre>
          <p className="ws-muted agent-approval-caveat">
            Secrets are removed and long payloads are shortened, so this may not be the
            complete input. Fingerprint <code>{approval.args_digest.slice(0, 12)}</code>.
          </p>
        </>
      )}

      {!readOnly && (
        <div className="agent-approval-actions">
          <fieldset className="agent-approval-remember">
            <legend>Apply this to</legend>
            <label>
              <input type="radio" name={`r-${approval.id}`} value="once"
                checked={remember === 'once'} onChange={() => setRemember('once')} />
              just this request
            </label>
            <label>
              <input type="radio" name={`r-${approval.id}`} value="session"
                checked={remember === 'session'} onChange={() => setRemember('session')} />
              every <code>{approval.signature}</code> until the agent restarts
            </label>
            <label>
              <input type="radio" name={`r-${approval.id}`} value="sandbox"
                checked={remember === 'sandbox'} onChange={() => setRemember('sandbox')} />
              every <code>{approval.signature}</code> for this workstation’s life
            </label>
          </fieldset>

          <div className="agent-approval-buttons">
            <button disabled={busy} onClick={() => onDecide('deny', 'once')}>
              {busy ? 'Sending…' : 'Refuse'}
            </button>
            <button className="ws-primary" disabled={busy}
              onClick={() => onDecide('allow', remember)}>
              {allowLabel(busy, remember)}
            </button>
          </div>
          {remember !== 'once' && (
            <p className="ws-muted agent-approval-note">
              Refusing only ever applies to this one request. You can take an “allow”
              back later from the policy list.
            </p>
          )}
        </div>
      )}
    </article>
  )
}

export function targetOf(a: AgentApproval): string {
  try {
    const input = JSON.parse(a.args_preview) as Record<string, unknown>
    for (const k of ['command', 'file_path', 'path', 'url', 'notebook_path', 'pattern']) {
      const v = input[k]
      if (typeof v === 'string' && v.trim()) return v
    }
  } catch {
  }
  return a.signature
}

export function useCountdown(requestedAt: string, timeoutSeconds: number): number | null {
  const [now, setNow] = useState(() => Date.now())
  const started = useRef(requestedAt)
  started.current = requestedAt

  useEffect(() => {
    if (!timeoutSeconds) return
    const iv = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(iv)
  }, [timeoutSeconds])

  if (!timeoutSeconds) return null
  const t = Date.parse(requestedAt)
  if (Number.isNaN(t)) return null
  return Math.max(0, Math.ceil((t + timeoutSeconds * 1000 - now) / 1000))
}

export function formatLeft(seconds: number): string {
  if (seconds >= 60) {
    const m = Math.floor(seconds / 60)
    const s = seconds % 60
    return s ? `${m}m ${s}s` : `${m}m`
  }
  return `${seconds}s`
}

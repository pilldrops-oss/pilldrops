import { useState } from 'react'
import { getAgentConnect, type AgentConnectBundle } from '../../api'

const LANGS = [
  { key: 'curl_submit', label: 'curl' },
  { key: 'node', label: 'Node' },
  { key: 'python', label: 'Python' },
] as const

export function ConnectCard({ sandboxID }: { sandboxID: string }) {
  const [bundle, setBundle] = useState<AgentConnectBundle | null>(null)
  const [lang, setLang] = useState<string>('curl_submit')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const load = async () => {
    setBusy(true)
    setErr(null)
    try {
      setBundle(await getAgentConnect(sandboxID))
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!bundle) {
    return (
      <section className="ws-card agent-connect">
        <h3>Connect</h3>
        <p className="ws-muted">
          Drive this agent from your terminal, your code, or an MCP client. This mints a
          short-lived token scoped to reading and sending tasks — not a shell, and not
          permission to change its spend cap.
        </p>
        <button className="ws-btn" onClick={() => void load()} disabled={busy}>
          {busy ? 'Minting…' : 'Get connection details'}
        </button>
        {err && <p className="ws-err" role="alert">{err}</p>}
      </section>
    )
  }

  const channels = bundle.channels ?? []

  return (
    <section className="ws-card agent-connect">
      <h3>Connect</h3>

      <div className="agent-connect-token">
        <p className="ws-warn" role="status">
          Copy this now: it is shown once. Asking again mints a different token.
        </p>
        <CopyRow label={`Token (${bundle.token.scopes.join(' + ')})`} value={bundle.token.value ?? ''} secret />
        <p className="ws-muted agent-connect-exp">
          Expires {new Date(bundle.token.expires_at).toLocaleString()}. Scoped to this
          workstation only.
        </p>
      </div>

      <h4>Send a task</h4>
      <p className="ws-muted">
        One request sends a task; the agent works even after you close this tab. You do not need
        any of this to use the Agent tab — it is for driving the agent from your own code.
      </p>
      <div className="agent-lang">
        {LANGS.filter(l => bundle.snippets[l.key]).map(l => (
          <button
            key={l.key}
            className={`ws-btn-sm${lang === l.key ? ' on' : ''}`}
            onClick={() => setLang(l.key)}
          >
            {l.label}
          </button>
        ))}
      </div>
      {bundle.snippets[lang] && <CopyBlock label="" value={bundle.snippets[lang]} />}
      {bundle.snippets.curl_watch && lang === 'curl_submit' && (
        <details className="agent-more">
          <summary>Watch it work (stream events)</summary>
          <CopyBlock label="" value={bundle.snippets.curl_watch} />
        </details>
      )}

      <details className="agent-more">
        <summary>Use it from an MCP client (optional)</summary>
        <CopyBlock label={`config (${bundle.mcp.transport})`} value={bundle.mcp.config} />
        {bundle.mcp.note && <p className="ws-muted">{bundle.mcp.note}</p>}
      </details>

      <details className="agent-more">
        <summary>Share this workstation with someone (optional)</summary>
        <CopyRow label="Viewer origin" value={bundle.origin.url} />
        <p className="ws-muted">{bundle.origin.note}</p>
      </details>

      <h4>Chat channels</h4>
      {channels.length === 0 ? (
        <p className="ws-muted">No chat channels are configured on this deployment.</p>
      ) : (
        <ul className="agent-connect-channels">
          {channels.map((c, i) => (
            <li key={c.id ?? i}>
              <span>{c.name ?? c.id}</span>{' '}
              {c.status === 'ready' ? (
                <span className="ws-ok">ready</span>
              ) : (
                <span className="ws-muted" title={c.reason}>
                  {c.status ?? 'unavailable'}{c.reason ? ` — ${c.reason}` : ''}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function CopyRow({ label, value, secret }: { label: string; value: string; secret?: boolean }) {
  const [revealed, setRevealed] = useState(false)
  const shown = secret && !revealed ? mask(value) : value
  return (
    <div className="agent-copy-row">
      <span className="agent-copy-label">{label}</span>
      <code className="agent-copy-value">{shown}</code>
      {secret && (
        <button className="ws-btn-sm" onClick={() => setRevealed(!revealed)}>
          {revealed ? 'Hide' : 'Reveal'}
        </button>
      )}
      <CopyButton value={value} />
    </div>
  )
}

function CopyBlock({ label, value }: { label: string; value: string }) {
  return (
    <div className="agent-copy-block">
      <div className="agent-copy-head">
        <span className="agent-copy-label">{label}</span>
        <CopyButton value={value} />
      </div>
      <pre><code>{value}</code></pre>
    </div>
  )
}

type CopyState = 'idle' | 'done' | 'failed'

const COPY_LABEL: Partial<Record<CopyState, string>> = { done: 'Copied', failed: 'Select it manually' }

function CopyButton({ value }: { value: string }) {
  const [state, setState] = useState<CopyState>('idle')
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setState('done')
    } catch {
      setState('failed')
    }
    setTimeout(() => setState('idle'), 2000)
  }
  return (
    <button className="ws-btn-sm" onClick={() => void copy()} aria-label="Copy">
      {COPY_LABEL[state] ?? 'Copy'}
    </button>
  )
}

function mask(v: string): string {
  if (v.length <= 12) return '••••••••'
  return `${v.slice(0, 8)}${'•'.repeat(12)}${v.slice(-4)}`
}

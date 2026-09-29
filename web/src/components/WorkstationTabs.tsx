import { useCallback, useEffect, useState } from 'react'
import {
  getSandboxConfig,
  setSandboxConfig,
  listIntegrations,
  getSandboxIntegrations,
  setSandboxIntegrations,
  getChannels,
  createPairingCode,
  revokeChatLinks,
} from '../api'
import type { ChannelInfo, EnvEntry, McpServer, SandboxConfig } from '../types'

function errText(e: unknown): string {
  const raw = e instanceof Error ? e.message : String(e)
  if (/404|not found/i.test(raw)) {
    return 'This deployment doesn’t serve workstation settings — the machine still works from its terminal.'
  }
  if (/fleet mode|orchestrator/i.test(raw)) {
    return 'This feature needs the hosted fleet, which this deployment isn’t connected to.'
  }
  return raw
}

function Panel({
  title,
  hint,
  children,
  busy,
  error,
  note,
}: {
  title: string
  hint?: string
  children: React.ReactNode
  busy?: boolean
  error?: string | null
  note?: string | null
}) {
  return (
    <section className="ws-panel">
      <h3>{title}</h3>
      {hint && <p className="ws-hint">{hint}</p>}
      {error && <div className="ws-error">{error}</div>}
      {note && <div className="ws-note">{note}</div>}
      <div style={{ opacity: busy ? 0.6 : 1 }}>{children}</div>
    </section>
  )
}

export function ModelTab({ id }: { id: string }) {
  const [cfg, setCfg] = useState<SandboxConfig | null>(null)
  const [model, setModel] = useState('')
  const [baseURL, setBaseURL] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      const c = await getSandboxConfig(id)
      setCfg(c)
      setModel(c.model)
      setBaseURL(c.base_url)
    } catch (e) {
      setError(errText(e))
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  async function save() {
    setBusy(true)
    setError(null)
    setNote(null)
    try {
      const c = await setSandboxConfig(id, { model: model.trim(), base_url: baseURL.trim() })
      setCfg(c)
      setNote('Saved — the next command the agent runs will use it. No restart needed.')
    } catch (e) {
      setError(errText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Panel
      title="Model"
      hint="Which model this workstation's agent uses, and optionally a gateway to reach it through. Applies to the next run — the machine keeps running."
      busy={busy}
      error={error}
      note={note}
    >
      <label className="ws-field">
        <span>Model</span>
        <input
          value={model}
          onChange={e => setModel(e.target.value)}
          placeholder="e.g. claude-sonnet-4-6"
        />
        <small>Leave empty to use whatever the agent defaults to.</small>
      </label>
      <label className="ws-field">
        <span>Base URL</span>
        <input
          value={baseURL}
          onChange={e => setBaseURL(e.target.value)}
          placeholder="https://your-gateway.example.com"
        />
        <small>Optional. Point the agent at a proxy or a self-hosted gateway.</small>
      </label>
      <button className="btn solid" onClick={() => void save()} disabled={busy}>
        {busy ? 'Saving…' : 'Save model settings'}
      </button>
      {cfg && (cfg.model || cfg.base_url) && (
        <p className="ws-current">
          Currently: <code>{cfg.model || 'agent default'}</code>
          {cfg.base_url && (
            <>
              {' via '}
              <code>{cfg.base_url}</code>
            </>
          )}
        </p>
      )}
      <p className="ws-current">
        No API key? Sign in with a subscription instead — one command in the terminal:{' '}
        <a href="/guides/claude-subscription" target="_blank" rel="noopener">Claude Pro/Max</a>
        {' · '}
        <a href="/guides/chatgpt-subscription" target="_blank" rel="noopener">ChatGPT Plus/Pro</a>
      </p>
    </Panel>
  )
}

export function KeysTab({ id }: { id: string }) {
  const [entries, setEntries] = useState<EnvEntry[]>([])
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      setEntries((await getSandboxConfig(id)).env)
    } catch (e) {
      setError(errText(e))
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  async function apply(patch: Record<string, string>, msg: string) {
    setBusy(true)
    setError(null)
    setNote(null)
    try {
      setEntries((await setSandboxConfig(id, { env: patch })).env)
      setNote(msg)
    } catch (e) {
      setError(errText(e))
    } finally {
      setBusy(false)
    }
  }

  async function add() {
    const n = name.trim()
    if (!n) return
    await apply({ [n]: value }, `${n} set — available to the next command.`)
    setName('')
    setValue('')
  }

  return (
    <Panel
      title="Keys & secrets"
      hint="Environment for this machine — the agent's API key, a database URL, tokens its integrations need. Takes effect on the next command."
      busy={busy}
      error={error}
      note={note}
    >
      {entries.length === 0 && <p className="ws-empty">Nothing set yet.</p>}
      {entries.length > 0 && (
        <table className="ws-table">
          <tbody>
            {entries.map(e => (
              <tr key={e.name}>
                <td>
                  <code>{e.name}</code>
                </td>
                <td className="ws-val">
                  {e.secret ? <span className="ws-masked">•••••••• set</span> : <code>{e.value}</code>}
                </td>
                <td>
                  <button
                    className="btn small"
                    disabled={busy}
                    onClick={() => void apply({ [e.name]: '' }, `${e.name} removed.`)}
                  >
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="ws-add">
        <input
          placeholder="NAME"
          value={name}
          onChange={e => setName(e.target.value.toUpperCase())}
          spellCheck={false}
        />
        <input
          placeholder="value"
          type="password"
          value={value}
          onChange={e => setValue(e.target.value)}
          spellCheck={false}
        />
        <button className="btn solid" onClick={() => void add()} disabled={busy || !name.trim()}>
          Add
        </button>
      </div>
      <small className="ws-hint">
        Letters, digits and underscore only. Setting an existing name replaces its value.
      </small>
    </Panel>
  )
}

export function IntegrationsTab({ id }: { id: string }) {
  const [catalog, setCatalog] = useState<McpServer[]>([])
  const [enabled, setEnabled] = useState<string[]>([])
  const [creds, setCreds] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      const [cat, mine] = await Promise.all([listIntegrations(), getSandboxIntegrations(id)])
      setCatalog(cat.servers)
      setEnabled(mine.servers ?? [])
    } catch (e) {
      setError(errText(e))
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  async function commit(next: string[]) {
    setBusy(true)
    setError(null)
    setNote(null)
    try {
      const res = await setSandboxIntegrations(id, next, creds)
      setEnabled(res.servers)
      setNote(res.note)
    } catch (e) {
      setError(errText(e))
      void load()
    } finally {
      setBusy(false)
    }
  }

  function toggle(sid: string) {
    const next = enabled.includes(sid) ? enabled.filter(x => x !== sid) : [...enabled, sid]
    if (next.length === 0) {
      setError('At least one integration must stay on. Remove them from the agent config directly if you want none.')
      return
    }
    void commit(next)
  }

  const needed = Array.from(
    new Set(catalog.filter(s => enabled.includes(s.id)).flatMap(s => s.env_keys ?? [])),
  )

  return (
    <Panel
      title="Integrations"
      hint="MCP servers this agent can reach. Any MCP server works — these are the ones we have wired up for you."
      busy={busy}
      error={error}
      note={note}
    >
      {needed.length > 0 && (
        <div className="ws-creds">
          <strong>Credentials these need</strong>
          {needed.map(k => (
            <label className="ws-field" key={k}>
              <span>{k}</span>
              <input
                type="password"
                placeholder="paste to set or replace"
                value={creds[k] ?? ''}
                onChange={e => setCreds({ ...creds, [k]: e.target.value })}
                spellCheck={false}
              />
            </label>
          ))}
          <button className="btn" disabled={busy} onClick={() => void commit(enabled)}>
            Save credentials
          </button>
        </div>
      )}

      <div className="ws-grid">
        {catalog.map(s => {
          const on = enabled.includes(s.id)
          return (
            <button
              key={s.id}
              className={`ws-card${on ? ' on' : ''}`}
              disabled={busy}
              onClick={() => toggle(s.id)}
            >
              <span className="ws-card-h">
                <b>{s.name}</b>
                <em>{on ? 'on' : 'off'}</em>
              </span>
              <span className="ws-card-d">{s.description}</span>
              {s.env_keys && s.env_keys.length > 0 && (
                <span className="ws-card-k">needs {s.env_keys.join(', ')}</span>
              )}
            </button>
          )
        })}
      </div>
    </Panel>
  )
}

export function ChannelsTab({ id }: { id: string }) {
  const [channels, setChannels] = useState<ChannelInfo[]>([])
  const [pair, setPair] = useState<{ code: string; url: string; channel: string } | null>(null)
  const [readOnly, setReadOnly] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [note, setNote] = useState<string | null>(null)

  const load = useCallback(async () => {
    try {
      setChannels((await getChannels(id)).channels)
    } catch (e) {
      setError(errText(e))
    }
  }, [id])

  useEffect(() => {
    void load()
  }, [load])

  async function connect(channel: string) {
    setBusy(true)
    setError(null)
    setNote(null)
    try {
      const l = await createPairingCode(id, { read_only: readOnly })
      setPair({ code: l.code, url: l.telegram_url, channel })
      setNote(`Single-use, expires in ${l.expires_in}.`)
    } catch (e) {
      setError(errText(e))
    } finally {
      setBusy(false)
    }
  }

  async function revoke() {
    setBusy(true)
    setError(null)
    try {
      await revokeChatLinks(id)
      setPair(null)
      setNote('Unlinked — that chat can no longer reach this workstation.')
      await load()
    } catch (e) {
      setError(errText(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Panel
      title="Channels"
      hint="Talk to this workstation from somewhere other than this page."
      busy={busy}
      error={error}
      note={note}
    >
      {channels.map(c => (
        <div className={`ws-chan${c.available ? '' : ' off'}`} key={c.id}>
          <div className="ws-chan-h">
            <b>{c.name}</b>
            {c.linked ? (
              <em className="on">linked{c.read_only ? ' · read-only' : ''}</em>
            ) : (
              <em>{c.available ? 'not connected' : 'unavailable'}</em>
            )}
          </div>
          <p className="ws-chan-d">{c.reason || c.setup}</p>

          {(c.id === 'telegram' || c.id === 'discord') && c.available && (
            <div className="ws-chan-a">
              {!c.linked && (
                <>
                  <label className="ws-check">
                    <input
                      type="checkbox"
                      checked={readOnly}
                      onChange={e => setReadOnly(e.target.checked)}
                    />
                    <span>Read-only — it can report, but never run anything</span>
                  </label>
                  <button className="btn solid" disabled={busy} onClick={() => void connect(c.id)}>
                    {c.id === 'telegram' ? 'Connect phone' : 'Connect Discord'}
                  </button>
                </>
              )}
              {c.linked && (
                <button className="btn" disabled={busy} onClick={() => void revoke()}>
                  Unlink {c.chat_id}
                </button>
              )}
            </div>
          )}

          {pair && pair.channel === c.id && c.id === 'telegram' && (
            <div className="ws-pair">
              {pair.url ? (
                <a className="btn solid" href={pair.url} target="_blank" rel="noreferrer">
                  Open Telegram →
                </a>
              ) : (
                <span className="ws-hint">
                  No bot username configured, so send the code to your bot manually:
                </span>
              )}
              <code className="ws-code">/link {pair.code}</code>
            </div>
          )}

          {pair && pair.channel === c.id && c.id === 'discord' && (
            <div className="ws-pair">
              <span className="ws-hint">DM the bot this command:</span>
              <code className="ws-code">!link {pair.code}</code>
            </div>
          )}
        </div>
      ))}
    </Panel>
  )
}

import { useState } from 'react'
import { useApi } from '../hooks/useApi'
import { listKeys, createKey, deleteKey } from '../api'
import type { ApiKey } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { RowsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'
import { errText } from '../lib/errText'

function toDate(ts: number | string): Date | null {
  const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts)
  return isNaN(d.getTime()) ? null : d
}

function formatDate(ts: number | string): string {
  const d = toDate(ts)
  if (!d) return String(ts)
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
}

function lastUsedLabel(ts: number | string): string {
  if (!ts) return 'Never'
  if (typeof ts === 'string' && (ts.startsWith('0001') || ts.startsWith('1970'))) return 'Never'
  const d = toDate(ts)
  if (!d || d.getUTCFullYear() <= 1970) return 'Never'
  return formatDate(ts)
}

export default function ApiKeys() {
  const { data: keys, loading, error, refetch } = useApi(listKeys)
  const toast = useToast()
  const confirm = useConfirm()
  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [newKey, setNewKey] = useState<{ key: string; name: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState(false)

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    if (!newName.trim()) return
    setBusy(true)
    try {
      const resp = await createKey(newName.trim())
      setNewKey({ key: resp.key, name: resp.name })
      setNewName('')
      setCreating(false)
      refetch()
    } catch (err) {
      toast.error(errText(err))
    } finally {
      setBusy(false)
    }
  }

  async function handleRevoke(key: ApiKey) {
    const ok = await confirm({
      title: 'Revoke API key?',
      body: <>Revoke <code>{key.name}</code>? Anything using it will stop working. This can’t be undone.</>,
      confirmLabel: 'Revoke',
      danger: true,
    })
    if (!ok) return
    try {
      await deleteKey(key.id)
      refetch()
    } catch (err) {
      toast.error(errText(err))
    }
  }

  function copyKey() {
    if (!newKey?.key) return
    navigator.clipboard.writeText(newKey.key).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    })
  }

  function keysBody() {
    if (loading && !keys) return <RowsSkeleton rows={3} />
    if ((keys?.length ?? 0) === 0) {
      return (
        <div className="empty">
          No API keys yet
          <p>Create one to authenticate with the pilldrops API.</p>
          <div className="keys-empty-cta">
            <button className="btn primary" onClick={() => setCreating(true)}>＋ Create your first key</button>
          </div>
        </div>
      )
    }
    return keys!.map(k => <KeyRow key={k.id} apiKey={k} onRevoke={handleRevoke} />)
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">API Keys</h1>
          <p>Manage bearer tokens for the pilldrops API</p>
        </div>
        <button className="btn primary" onClick={() => setCreating(c => !c)}>
          {creating ? 'Cancel' : '＋ Create key'}
        </button>
      </div>

      {creating && (
        <div className="callout" style={{ marginBottom: 20, maxWidth: 480 }}>
          <form onSubmit={handleCreate} className="keys-form">
            <div className="form-group keys-form-field">
              <label htmlFor="keyname">Key name</label>
              <input
                id="keyname"
                type="text"
                placeholder="My API key"
                value={newName}
                onChange={e => setNewName(e.target.value)}
                required
                autoFocus
              />
            </div>
            <button type="submit" className="btn primary" disabled={busy}>
              {busy ? 'Creating…' : 'Create'}
            </button>
          </form>
        </div>
      )}

      {newKey && (
        <div className="callout success" style={{ marginBottom: 20, maxWidth: 560 }}>
          <strong style={{ color: 'var(--green)' }}>Key created: {newKey.name}</strong>
          <p style={{ margin: '6px 0 10px', color: 'var(--sub)', fontSize: 13 }}>
            Copy it now — you won't see the full key again.
          </p>
          <div className="keys-created">
            <code style={{
              flex: 1,
              fontFamily: 'var(--mono)',
              fontSize: 12.5,
              background: 'var(--surface)',
              border: '1px solid var(--line)',
              borderRadius: 8,
              padding: '8px 12px',
              wordBreak: 'break-all',
              color: 'var(--ink)',
            }}>
              {newKey.key}
            </code>
            <button className="copy-btn" onClick={copyKey}>
              {copied ? '✓' : 'Copy'}
            </button>
          </div>
          <button
            className="link-btn"
            style={{ marginTop: 10 }}
            onClick={() => setNewKey(null)}
          >
            Dismiss
          </button>
        </div>
      )}

      {error != null && (
        <div style={{ marginBottom: 20 }}>
          <ErrorState error={error} onRetry={refetch} title="Couldn’t load keys" />
        </div>
      )}

      <div className="panel">
        <div className="ph">
          <h2>Keys</h2>
          <span className="meta">{keys?.length ?? 0} total</span>
        </div>

        <div className="keys-row head">
          <div>Name</div>
          <div>Key</div>
          <div>Created</div>
          <div>Last used</div>
          <div />
        </div>

        {keysBody()}
      </div>
    </div>
  )
}

function KeyRow({ apiKey, onRevoke }: { apiKey: ApiKey; onRevoke: (k: ApiKey) => void }) {
  return (
    <div className="keys-row keys-row-sep">
      <div style={{ fontWeight: 600 }}>{apiKey.name}</div>
      <div style={{ fontFamily: 'var(--mono)', fontSize: 12.5, color: 'var(--sub)' }}>
        {apiKey.masked_key}
      </div>
      <div style={{ fontSize: 12.5, color: 'var(--dim)' }}>
        {formatDate(apiKey.created_at)}
      </div>
      <div style={{ fontSize: 12.5, color: 'var(--dim)' }}>
        {lastUsedLabel(apiKey.last_used)}
      </div>
      <div className="keys-row-act">
        <button
          className="iconbtn"
          title="Revoke key"
          aria-label={`Revoke key ${apiKey.name}`}
          onClick={() => onRevoke(apiKey)}
          style={{ color: '#dc2626' }}
        >
          ✕
        </button>
      </div>
    </div>
  )
}

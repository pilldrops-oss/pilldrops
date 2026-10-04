import { useCallback, useEffect, useRef, useState } from 'react'
import { listSecrets, putSecret, deleteSecret, setSecretAutoInject } from '../api'
import type { VaultSecret } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'

// Vault — org-scoped named secrets, referenced by name at launch. Values are
// AES-GCM encrypted at rest and never readable back from the API.

const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]{0,63}$/

export default function Vault() {
  const toast = useToast()
  const confirm = useConfirm()

  const [secrets, setSecrets] = useState<VaultSecret[]>([])
  const [loading, setLoading] = useState(true)
  const [loadErr, setLoadErr] = useState('')
  const [busy, setBusy] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      setSecrets(await listSecrets())
      setLoadErr('')
    } catch (e) {
      setLoadErr(errText(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { refresh() }, [refresh])

  const store = useCallback(async (name: string, value: string, autoInject: boolean | undefined, done: string) => {
    setBusy(name)
    try {
      await putSecret(name, value, autoInject)
      await refresh()
      toast.success(done)
      return true
    } catch (e) {
      toast.error(`Could not save ${name}: ${errText(e)}`)
      return false
    } finally {
      setBusy(null)
    }
  }, [refresh, toast])

  // Flips auto-inject on its own, without touching the secret's value.
  const toggleAutoInject = useCallback(async (name: string, autoInject: boolean) => {
    setBusy(name)
    try {
      await setSecretAutoInject(name, autoInject)
      await refresh()
    } catch (e) {
      toast.error(`Could not update ${name}: ${errText(e)}`)
    } finally {
      setBusy(null)
    }
  }, [refresh, toast])

  const remove = useCallback(async (name: string, title: string, body: React.ReactNode) => {
    const ok = await confirm({ title, body, confirmLabel: 'Remove', danger: true })
    if (!ok) return
    setBusy(name)
    try {
      await deleteSecret(name)
      await refresh()
      toast.success(`Removed ${name}`)
    } catch (e) {
      toast.error(`Could not remove ${name}: ${errText(e)}`)
    } finally {
      setBusy(null)
    }
  }, [confirm, refresh, toast])

  return (
    // `wrap` is the shared page frame (padding + house max-width). Without it
    // this page rendered flush against the sidebar with no padding at all,
    // unlike every other route.
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Vault</h1>
          <p>
            Secrets are encrypted at rest, injected server-side at launch, and never readable back.
          </p>
        </div>
      </div>

      {loadErr && <p className="lm-error">Could not load the vault: {loadErr}</p>}

      <CustomSecrets
        secrets={secrets}
        busy={busy}
        onStore={store}
        onRemove={remove}
        onToggleAutoInject={toggleAutoInject}
        loading={loading}
      />
    </div>
  )
}

// ── Custom secrets ───────────────────────────────────────────────────────────

function CustomSecrets({
  secrets, busy, onStore, onRemove, onToggleAutoInject, loading,
}: {
  secrets: VaultSecret[]
  busy: string | null
  onStore: (name: string, value: string, autoInject: boolean | undefined, done: string) => Promise<boolean>
  onRemove: (name: string, title: string, body: React.ReactNode) => Promise<void>
  onToggleAutoInject: (name: string, autoInject: boolean) => Promise<void>
  loading: boolean
}) {
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [autoInject, setAutoInject] = useState(false)
  const [error, setError] = useState('')
  const valueRef = useRef<HTMLInputElement>(null)

  const trimmed = name.trim()
  const replacing = secrets.some(s => s.name === trimmed)
  // Checked here as well as server-side so the message lands next to the field
  // rather than arriving as a failed request.
  const nameBad = trimmed !== '' && !ENV_NAME_RE.test(trimmed)

  async function add() {
    if (!trimmed || !value) return
    if (!ENV_NAME_RE.test(trimmed)) {
      setError('Use an ENV_VAR-style name: A–Z, 0–9 and underscores, starting with a letter.')
      return
    }
    setError('')
    // A rotation only changes the value: auto-inject is its own toggle on the
    // row now, and omitting the field here keeps whatever it is set to.
    const ok = await onStore(trimmed, value, replacing ? undefined : autoInject, `${replacing ? 'Replaced' : 'Added'} ${trimmed}`)
    if (ok) {
      setName('')
      setValue('')
      setAutoInject(false)
    }
  }

  return (
    <div className="panel vault-panel">
      <div className="lm-sec-h vault-sec-h">
        Custom secrets{' '}
        <span className="vault-sec-note">any ENV_VAR-named value, referenced at launch</span>
      </div>

      {!loading && secrets.length === 0 && (
        <p className="vault-empty">
          Nothing yet. Add anything your sandboxes need as an env var — a database URL, a
          deploy token, a webhook secret.
        </p>
      )}

      {secrets.map(s => (
        <div key={s.name} className="vault-custom-row">
          <code className="chip mono">{s.name}</code>
          <span className="vault-last4">…{s.last4}</span>
          <label className="chip" title="Injected into every new sandbox without a secret_refs entry">
            <input
              type="checkbox"
              aria-label={`Auto-inject ${s.name}`}
              checked={!!s.auto_inject}
              disabled={busy === s.name}
              onChange={e => onToggleAutoInject(s.name, e.target.checked)}
            />{' '}
            auto-inject
          </label>
          <button
            type="button"
            className="btn ghost"
            disabled={busy === s.name}
            onClick={() => {
              setName(s.name); setValue(''); setError('')
              valueRef.current?.focus()
            }}
          >
            Replace value
          </button>
          <button
            type="button"
            className="btn ghost"
            disabled={busy === s.name}
            onClick={() =>
              onRemove(
                s.name,
                `Remove ${s.name}?`,
                <>Launches referencing <code>{s.name}</code> will no longer receive it.</>,
              )
            }
          >
            Remove
          </button>
        </div>
      ))}

      <div className="vault-add">
        <input
          className="pa-input mono vault-add-name"
          placeholder="SECRET_NAME"
          aria-label="Secret name"
          value={name}
          onChange={e => { setName(e.target.value.toUpperCase()); setError('') }}
        />
        <input
          className="pa-input mono vault-input"
          type="password"
          autoComplete="off"
          ref={valueRef}
          placeholder={replacing ? `new value for ${trimmed}` : 'value'}
          aria-label="Secret value"
          value={value}
          onChange={e => setValue(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') add() }}
        />
        {!replacing && (
          <label className="vault-auto">
            <input type="checkbox" checked={autoInject} onChange={e => setAutoInject(e.target.checked)} />{' '}
            Inject into every new sandbox
          </label>
        )}
        <button
          type="button"
          className="btn primary"
          disabled={!!busy || !trimmed || !value || nameBad}
          onClick={add}
        >
          {replacing ? 'Replace secret' : 'Add secret'}
        </button>
      </div>
      {(error || nameBad) && (
        <p className="lm-error vault-gw-msg">
          {error || 'Use an ENV_VAR-style name: A–Z, 0–9 and underscores, starting with a letter.'}
        </p>
      )}
    </div>
  )
}

import { useCallback, useEffect, useState } from 'react'
import { listSecrets, putSecret, deleteSecret } from '../api'
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

  const store = useCallback(async (name: string, value: string, done: string) => {
    setBusy(name)
    try {
      await putSecret(name, value)
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
        loading={loading}
      />
    </div>
  )
}

// ── Custom secrets ───────────────────────────────────────────────────────────

function CustomSecrets({
  secrets, busy, onStore, onRemove, loading,
}: {
  secrets: VaultSecret[]
  busy: string | null
  onStore: (name: string, value: string, done: string) => Promise<boolean>
  onRemove: (name: string, title: string, body: React.ReactNode) => Promise<void>
  loading: boolean
}) {
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')

  const trimmed = name.trim()
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
    const ok = await onStore(trimmed, value, `Added ${trimmed}`)
    if (ok) {
      setName('')
      setValue('')
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
          placeholder="value"
          aria-label="Secret value"
          value={value}
          onChange={e => setValue(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') add() }}
        />
        <button
          type="button"
          className="btn primary"
          disabled={!!busy || !trimmed || !value || nameBad}
          onClick={add}
        >
          Add secret
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

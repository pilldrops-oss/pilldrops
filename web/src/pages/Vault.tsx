import { useCallback, useEffect, useState } from 'react'
import { listSecrets, putSecret, deleteSecret } from '../api'
import type { VaultSecret } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'
import { validateGatewayUrl } from '../lib/gatewayUrl'
import { PROVIDER_SETTINGS, managedVaultNames, type ProviderSetting } from '../lib/providerSettings'

// Vault — org-scoped provider keys and named secrets. Connect a provider once
// and every matching agent launch becomes zero-field: the launch form shows
// "key: connected" and the portal injects the value server-side (secret_refs).
// Values are AES-GCM encrypted at rest and never readable back from the API.
//
// Gateway settings are the exception: a base URL and a model name are
// configuration rather than credentials, so the server returns them in full
// (cmd/devserver/vault.go, kind "env") and they are shown and editable
// here. A setting you cannot read back is one you cannot verify or correct.

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

  const byName = new Map(secrets.map(s => [s.name, s]))

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

  const managed = managedVaultNames()
  const customSecrets = secrets.filter(s => !managed.has(s.name))

  return (
    // `wrap` is the shared page frame (padding + house max-width). Without it
    // this page rendered flush against the sidebar with no padding at all,
    // unlike every other route.
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Vault</h1>
          <p>
            Connect a provider once — matching agent launches become one click. Keys are
            encrypted at rest, injected server-side at launch, and never readable back.
          </p>
        </div>
      </div>

      {loadErr && <p className="lm-error">Could not load the vault: {loadErr}</p>}

      <div className="panel vault-panel">
        <div className="lm-sec-h vault-sec-h">Providers</div>

        {loading ? (
          <div className="vault-loading">Loading…</div>
        ) : (
          PROVIDER_SETTINGS.map(p => (
            <ProviderRow
              key={p.keyEnv}
              provider={p}
              secret={byName.get(p.keyEnv)}
              gatewaySecret={p.baseUrlEnv ? byName.get(p.baseUrlEnv) : undefined}
              modelSecret={p.modelEnv ? byName.get(p.modelEnv) : undefined}
              busy={busy}
              onStore={store}
              onRemove={remove}
            />
          ))
        )}
      </div>

      <CustomSecrets
        secrets={customSecrets}
        busy={busy}
        onStore={store}
        onRemove={remove}
        loading={loading}
      />
    </div>
  )
}

// ── Provider row ─────────────────────────────────────────────────────────────

function saveLabel(saving: boolean, replacing: boolean): string {
  if (saving) return 'Saving…'
  return replacing ? 'Save' : 'Connect'
}

function ProviderRow({
  provider, secret, gatewaySecret, modelSecret, busy, onStore, onRemove,
}: {
  provider: ProviderSetting
  secret?: VaultSecret
  gatewaySecret?: VaultSecret
  modelSecret?: VaultSecret
  busy: string | null
  onStore: (name: string, value: string, done: string) => Promise<boolean>
  onRemove: (name: string, title: string, body: React.ReactNode) => Promise<void>
}) {
  const [draft, setDraft] = useState('')
  const [replacing, setReplacing] = useState(false)
  const connected = !!secret
  const editingKey = !connected || replacing
  const gatewayOn = !!gatewaySecret

  function gatewayArea() {
    if (provider.baseUrlEnv) {
      return (
        <GatewaySettings
          provider={provider}
          baseUrl={gatewaySecret?.value ?? ''}
          model={modelSecret?.value ?? ''}
          active={gatewayOn}
          busy={busy}
          onStore={onStore}
          onRemove={onRemove}
        />
      )
    }
    // Better to say why there is no field than to leave a silent gap that
    // reads as an oversight.
    if (provider.note) return <p className="vault-gw-none">{provider.note}</p>
    return null
  }

  async function saveKey() {
    if (!draft.trim()) return
    const ok = await onStore(provider.keyEnv, draft.trim(), `${provider.label} connected`)
    if (ok) {
      setDraft('')
      setReplacing(false)
    }
  }

  return (
    <div className="vault-row">
      <div className="vault-row-main">
        <div className="vault-id">
          <span className="vault-name">{provider.label}</span>
          <span className="vault-env">
            <code>{provider.keyEnv}</code> · used by {provider.usedBy}
          </span>
        </div>

        {editingKey ? (
          <div className="vault-field">
            <input
              className="pa-input mono vault-input"
              type="password"
              autoComplete="off"
              placeholder={provider.hint}
              aria-label={`${provider.label} API key`}
              value={draft}
              onChange={e => setDraft(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') saveKey() }}
            />
            <button
              type="button"
              className="btn primary"
              disabled={busy === provider.keyEnv || !draft.trim()}
              onClick={saveKey}
            >
              {saveLabel(busy === provider.keyEnv, replacing)}
            </button>
            {replacing && (
              <button type="button" className="btn ghost" onClick={() => { setReplacing(false); setDraft('') }}>
                Cancel
              </button>
            )}
          </div>
        ) : (
          <div className="vault-field">
            <span className="chip mono vault-ok" title={`updated ${secret!.updated_at}`}>
              ✓ connected · …{secret!.last4}
            </span>
            {/* Replacing a rotated key used to mean disconnecting first, which
                left every launch keyless in between. */}
            <button type="button" className="btn ghost" onClick={() => setReplacing(true)}>
              Replace
            </button>
            <button
              type="button"
              className="btn ghost"
              disabled={busy === provider.keyEnv}
              onClick={() =>
                onRemove(
                  provider.keyEnv,
                  `Disconnect ${provider.label}?`,
                  <>Launches that rely on <code>{provider.keyEnv}</code> will need a key again.</>,
                )
              }
            >
              Disconnect
            </button>
          </div>
        )}
      </div>

      {gatewayArea()}
    </div>
  )
}

// ── Gateway settings ─────────────────────────────────────────────────────────

function GatewaySettings({
  provider, baseUrl, model, active, busy, onStore, onRemove,
}: {
  provider: ProviderSetting
  baseUrl: string
  model: string
  active: boolean
  busy: string | null
  onStore: (name: string, value: string, done: string) => Promise<boolean>
  onRemove: (name: string, title: string, body: React.ReactNode) => Promise<void>
}) {
  // Only rendered when baseUrlEnv exists, so these are safe to assert.
  const gw = {
    baseUrlEnv: provider.baseUrlEnv!,
    modelEnv: provider.modelEnv,
    defaultBase: provider.defaultBase ?? '',
    modelHint: provider.modelHint ?? '',
  }
  const [url, setUrl] = useState(baseUrl)
  const [mdl, setMdl] = useState(model)
  const [error, setError] = useState('')
  const [note, setNote] = useState('')

  // Re-sync when a refresh brings new values in.
  useEffect(() => { setUrl(baseUrl) }, [baseUrl])
  useEffect(() => { setMdl(model) }, [model])

  const dirty = url.trim() !== baseUrl || (!!gw.modelEnv && mdl.trim() !== model)

  async function save() {
    setError('')
    setNote('')
    const check = validateGatewayUrl(url)
    if (!check.ok) {
      setError(check.error)
      return
    }
    if (check.note) setNote(check.note)
    const ok = await onStore(gw.baseUrlEnv, check.url, `${provider.label} gateway saved`)
    if (!ok) return
    setUrl(check.url)
    // The model is optional, and not every provider has a model variable at
    // all — grok's CLI reads a base URL but no model env var.
    if (gw.modelEnv && mdl.trim() && mdl.trim() !== model) {
      await onStore(gw.modelEnv, mdl.trim(), `${provider.label} model set to ${mdl.trim()}`)
    }
  }

  return (
    <details className="vault-gw" open={active}>
      <summary className="vault-gw-sum">
        {active ? (
          <>
            <span className="vault-gw-badge">gateway</span>
            <code className="vault-gw-url">{baseUrl}</code>
          </>
        ) : (
          <span className="vault-gw-hint">Using a gateway or proxy instead of {gw.defaultBase}?</span>
        )}
      </summary>

      <div className="vault-gw-body">
        <p className="vault-gw-copy">
          Point {provider.label} at your own endpoint — a corporate proxy, LiteLLM,
          OpenRouter, or any {provider.label}-compatible gateway. Leave blank to call{' '}
          <code>{gw.defaultBase}</code> directly.
        </p>
        {/* The two consumers are genuinely different, and a user who sets a URL
            expecting it to affect the other would be quietly disappointed. */}
        <p className="vault-gw-copy vault-gw-scope">
          Applies to{' '}
          {provider.baseUrlUse?.includes('configure') && <>“Describe it” launches</>}
          {provider.baseUrlUse?.length === 2 && <> and </>}
          {provider.baseUrlUse?.includes('launch') && <>{provider.usedBy} sandboxes</>}
          .
        </p>

        <div className="vault-gw-grid">
          <label className="vault-gw-lbl" htmlFor={`${gw.baseUrlEnv}-url`}>
            Base URL <code>{gw.baseUrlEnv}</code>
          </label>
          <input
            id={`${gw.baseUrlEnv}-url`}
            className="pa-input mono vault-input"
            placeholder={gw.defaultBase}
            value={url}
            onChange={e => { setUrl(e.target.value); setError(''); setNote('') }}
            onKeyDown={e => { if (e.key === 'Enter') save() }}
          />

          {gw.modelEnv && (
            <>
              <label className="vault-gw-lbl" htmlFor={`${gw.modelEnv}-model`}>
                Model <span className="vault-gw-opt">optional</span>
                <code>{gw.modelEnv}</code>
              </label>
              <input
                id={`${gw.modelEnv}-model`}
                className="pa-input mono vault-input"
                placeholder={gw.modelHint}
                value={mdl}
                onChange={e => setMdl(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter') save() }}
              />
            </>
          )}
        </div>

        {error && <p className="lm-error vault-gw-msg">{error}</p>}
        {!error && note && <p className="vault-gw-note">{note}</p>}

        <div className="vault-gw-actions">
          <button
            type="button"
            className="btn primary"
            disabled={!dirty || busy === gw.baseUrlEnv}
            onClick={save}
          >
            {busy === gw.baseUrlEnv ? 'Saving…' : 'Save gateway'}
          </button>
          {active && (
            <button
              type="button"
              className="btn ghost"
              disabled={busy === gw.baseUrlEnv}
              onClick={() =>
                onRemove(
                  gw.baseUrlEnv,
                  'Use the provider directly?',
                  <>{provider.label} will go back to <code>{gw.defaultBase}</code>.</>,
                )
              }
            >
              Use direct
            </button>
          )}
        </div>
      </div>
    </details>
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

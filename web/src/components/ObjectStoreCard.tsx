import { useCallback, useEffect, useState } from 'react'
import { deleteObjectStore, getObjectStore, putObjectStore } from '../api'
import type { ObjectStoreView } from '../types'
import { useToast } from './Toast'
import { useConfirm } from './ConfirmModal'
import { errText } from '../lib/errText'

const empty = { endpoint: '', bucket: '', region: '', access_key: '', secret_key: '' }

const fields: { key: keyof typeof empty; label: string; placeholder: string; secret?: boolean }[] = [
  { key: 'endpoint', label: 'Endpoint', placeholder: 'https://s3.eu-west-3.amazonaws.com' },
  { key: 'bucket', label: 'Bucket', placeholder: 'bucket name' },
  { key: 'region', label: 'Region', placeholder: 'eu-west-3' },
  { key: 'access_key', label: 'Access key', placeholder: 'access key', secret: true },
  { key: 'secret_key', label: 'Secret key', placeholder: 'secret key', secret: true },
]

export default function ObjectStoreCard() {
  const toast = useToast()
  const confirm = useConfirm()
  const [view, setView] = useState<ObjectStoreView | null>(null)
  const [loadErr, setLoadErr] = useState('')
  const [form, setForm] = useState(empty)
  const [editing, setEditing] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    getObjectStore().then(setView).catch(e => setLoadErr(errText(e)))
  }, [])

  const save = useCallback(async () => {
    setBusy(true)
    setError('')
    try {
      setView(await putObjectStore(form))
      // The keys leave this page's memory the moment they are saved; the server never returns them.
      setForm(empty)
      setEditing(false)
      toast.success('Object storage saved. Restart each node to apply it.')
    } catch (e) {
      setError(errText(e))
    } finally {
      setBusy(false)
    }
  }, [form, toast])

  const remove = useCallback(async () => {
    const ok = await confirm({
      title: 'Remove object storage?',
      body: 'Backups, image builds, named snapshots and moves stop working until a bucket is saved again.',
      confirmLabel: 'Remove',
      danger: true,
    })
    if (!ok) return
    setBusy(true)
    try {
      setView(await deleteObjectStore())
      toast.success('Object storage removed')
    } catch (e) {
      toast.error(`Could not remove object storage: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }, [confirm, toast])

  const saved = view?.source === 'saved'
  const showForm = view && view.source !== 'env' && (!saved || editing)

  return (
    <div className="panel" style={{ marginBottom: 16 }}>
      <div className="ph">
        <h2>Object storage</h2>
        <span className="meta">one bucket for the control plane and every node</span>
      </div>
      <div className="sys-zone-body">
        {loadErr && <p className="lm-error">Could not load object storage: {loadErr}</p>}
        {view?.source === 'env' && (
          <p className="vault-empty">
            Set by the control plane's <code>ORCHESTRATOR_ARTIFACT_*</code> environment variables. Remove them and
            restart the control plane to manage it here.
          </p>
        )}
        {saved && (
          <p className="vault-empty">
            <code className="chip mono">{view.bucket}</code> at <code className="chip mono">{view.endpoint}</code>
            {view.region && <> · {view.region}</>} · access key ending <code className="chip mono">{view.access_key_hint}</code>
            {' '}· secret key saved, cannot be shown
          </p>
        )}
        {view && !view.configured && (
          <p className="vault-empty">
            No bucket yet, so backups, image builds, named snapshots and moves are off. The keys are tested, then
            stored encrypted and never shown again.
          </p>
        )}
        {saved && !editing && (
          <div className="vault-add">
            <button type="button" className="btn" disabled={busy} onClick={() => setEditing(true)}>Replace</button>
            <button type="button" className="btn ghost" disabled={busy} onClick={remove}>Remove</button>
          </div>
        )}
        {showForm && (
          <div className="vault-add">
            {fields.map(f => (
              <input
                key={f.key}
                className="pa-input mono"
                type={f.secret ? 'password' : 'text'}
                autoComplete="off"
                placeholder={f.placeholder}
                aria-label={f.label}
                value={form[f.key]}
                onChange={e => setForm({ ...form, [f.key]: e.target.value.trim() })}
              />
            ))}
            <button
              type="button"
              className="btn primary"
              disabled={busy || !form.endpoint || !form.bucket || !form.access_key || !form.secret_key}
              onClick={save}
            >
              {busy ? 'Testing…' : 'Test and save'}
            </button>
            {editing && (
              <button type="button" className="btn ghost" disabled={busy} onClick={() => { setEditing(false); setForm(empty); setError('') }}>
                Cancel
              </button>
            )}
          </div>
        )}
        {error && <p className="lm-error vault-gw-msg">{error}</p>}
      </div>
    </div>
  )
}

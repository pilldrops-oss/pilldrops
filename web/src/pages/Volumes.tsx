import { useCallback, useEffect, useState } from 'react'
import { createVolume, deleteVolume, listVolumes } from '../api'
import type { Volume } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'

const gb = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(mb % 1024 ? 1 : 0)} GB` : `${mb} MB`)

export default function Volumes() {
  const [volumes, setVolumes] = useState<Volume[] | null>(null)
  const [loadErr, setLoadErr] = useState('')
  const [name, setName] = useState('')
  const [sizeGB, setSizeGB] = useState(10)
  const toast = useToast()
  const confirm = useConfirm()

  const load = useCallback(() => {
    listVolumes().then(v => { setVolumes(v); setLoadErr('') }).catch(e => setLoadErr(errText(e)))
  }, [])
  useEffect(load, [load])

  async function act(fn: () => Promise<unknown>, ok: string) {
    try {
      await fn()
      toast.success(ok)
      load()
    } catch (e) {
      toast.error(errText(e))
    }
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Volumes</h1>
          <p>Disks that outlive the sandbox they are attached to</p>
        </div>
      </div>
      {loadErr ? <p>{loadErr}</p> : (
        <>
          <form className="volume-form"
            onSubmit={e => { e.preventDefault(); void act(() => createVolume(name.trim(), sizeGB * 1024), 'Volume created').then(() => setName('')) }}>
            <input className="inp" placeholder="name" aria-label="Volume name" value={name} onChange={e => setName(e.target.value)} />
            <input className="inp" type="number" min={1} max={100} aria-label="Size in GB" value={sizeGB}
              onChange={e => setSizeGB(Number(e.target.value))} style={{ width: 90 }} />
            <span>GB</span>
            <button className="btn primary" type="submit">Create volume</button>
          </form>
          <table className="tbl panel">
            <thead><tr><th>Name</th><th>ID</th><th>Size</th><th>Attached to</th><th /></tr></thead>
            <tbody>
              {(volumes ?? []).map(v => (
                <tr key={v.id}>
                  <td>{v.name || '—'}</td>
                  <td className="mono">{v.id}</td>
                  <td>{gb(v.size_mb)}</td>
                  <td className="mono">{v.attached_to || '—'}</td>
                  <td>
                    <button className="btn ghost" disabled={!!v.attached_to}
                      title={v.attached_to ? 'Destroy the sandbox holding it first' : undefined}
                      onClick={async () => {
                        if (await confirm({ title: 'Delete volume?', body: `Delete ${v.name || v.id} and everything on it? This cannot be undone.`, confirmLabel: 'Delete', danger: true })) {
                          void act(() => deleteVolume(v.id), 'Volume deleted')
                        }
                      }}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
              {volumes?.length === 0 && <tr><td colSpan={5}>No volumes yet. Create one above, then attach it at create with volumes: [{'{'}id, path{'}'}].</td></tr>}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}

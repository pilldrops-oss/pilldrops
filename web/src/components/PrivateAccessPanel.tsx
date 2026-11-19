import { Link } from 'react-router-dom'
import { useCallback, useEffect, useRef, useState } from 'react'
import { listPrivateGrants, grantPrivate, revokePrivate, listConnectors } from '../api'
import type { Connector, PrivateGrant } from '../types'
import { useToast } from './Toast'
import { useConfirm } from './ConfirmModal'
import { errText } from '../lib/errText'
import { useOrgRole } from '../hooks/useOrgRole'

export default function PrivateAccessPanel({ sandboxId }: { sandboxId: string }) {
  const toast = useToast()
  const toastRef = useRef(toast)
  toastRef.current = toast
  const confirm = useConfirm()
  const role = useOrgRole()
  const canChange = role === 'owner' || role === 'admin'
  const [grants, setGrants] = useState<PrivateGrant[] | null>(null)
  const [supported, setSupported] = useState(true)
  const [hidden, setHidden] = useState(false)
  const [connectors, setConnectors] = useState<Connector[]>([])
  const [adding, setAdding] = useState(false)
  const [pick, setPick] = useState({ connector: '', target: '' })

  const load = useCallback(async () => {
    try { const r = await listPrivateGrants(sandboxId); setGrants(r.grants); setSupported(r.supported) }
    catch (e) { if ((e as { status?: number }).status === 501) setHidden(true); else toastRef.current.error(errText(e)) }
  }, [sandboxId])
  useEffect(() => { void load() }, [load])
  useEffect(() => { if (canChange) listConnectors().then(setConnectors).catch(e => toastRef.current.error(errText(e))) }, [canChange])

  if (hidden || grants === null) return null
  const targets = connectors.find(c => c.name === pick.connector)?.targets ?? []

  return (
    <section className="panel" aria-label="Private access">
      <div className="ph"><h2>Private access</h2><span className="meta">hosts on your own network, through a connector</span></div>
      <div className="panel-pad">
        {!supported && <p className="muted">Private access is not available on this sandbox's node.</p>}
        {grants.length === 0 ? <p className="vault-empty">This sandbox reaches no private targets.</p> : (
          <table className="tbl"><tbody>{grants.map(g => (
            <tr key={g.target}>
              <td>{g.names.map(n => <code key={n}>{n}</code>)}</td>
              <td>{g.connector}{g.online ? '' : ' (offline)'}</td>
              {canChange && <td><button className="btn ghost" aria-label={`Remove ${g.target}`} onClick={async () => {
                if (!await confirm({ title: `Remove ${g.target}.private?`, body: 'Open connections to it close.', danger: true })) return
                try { await revokePrivate(sandboxId, g.connector, g.target); await load() } catch (e) { toast.error(errText(e)) }
              }}>Remove</button></td>}
            </tr>))}</tbody></table>
        )}
        {canChange && connectors.length === 0 && <p className="vault-empty">No connectors yet. <Link to="/connectors">Create one</Link>.</p>}
        {canChange && connectors.length > 0 && (
          <form className="vault-add" onSubmit={async e => {
            e.preventDefault()
            setAdding(true)
            try { await grantPrivate(sandboxId, pick.connector, pick.target); setPick({ connector: '', target: '' }); await load() }
            catch (err) { toast.error(errText(err)) } finally { setAdding(false) }
          }}>
            <select className="sel" aria-label="Connector" value={pick.connector} disabled={!supported} onChange={e => setPick({ connector: e.target.value, target: '' })}>
              <option value="">Connector…</option>
              {connectors.map(c => <option key={c.id} value={c.name}>{c.name}</option>)}
            </select>
            <select className="sel" aria-label="Target" value={pick.target} disabled={!supported || !pick.connector} onChange={e => setPick({ ...pick, target: e.target.value })}>
              <option value="">Target…</option>
              {targets.map(t => <option key={t.name} value={t.name}>{t.name}:{t.port}</option>)}
            </select>
            <button className="btn" type="submit" disabled={!supported || !pick.target || adding}>Add</button>
          </form>
        )}
      </div>
    </section>
  )
}

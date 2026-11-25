import { Link } from 'react-router-dom'
import { useCallback, useEffect, useRef, useState } from 'react'
import { listPrivateGrants, grantPrivate, revokePrivate, listConnectors } from '../api'
import type { Connector, GrantKind, PrivateGrant } from '../types'
import { useToast } from './Toast'
import { useConfirm } from './ConfirmModal'
import { errText } from '../lib/errText'
import { useOrgRole } from '../hooks/useOrgRole'

const KIND_LABEL: Record<GrantKind, string> = { target: 'Target', subnet: 'Subnet', dns: 'DNS' }
const EMPTY_PICK = { connector: '', kind: 'target' as GrantKind, value: '' }

/** What a grant answers to inside the sandbox. */
const reachedAs = (g: PrivateGrant) => (g.kind === 'target' ? g.names ?? [`${g.value}.private`] : [g.value])

export default function PrivateAccessPanel({ sandboxId }: { sandboxId: string }) {
  const toast = useToast()
  const toastRef = useRef(toast)
  toastRef.current = toast
  const confirm = useConfirm()
  const role = useOrgRole()
  const canChange = role === 'owner' || role === 'admin'
  const [grants, setGrants] = useState<PrivateGrant[] | null>(null)
  const [supported, setSupported] = useState(true)
  const [routesSupported, setRoutesSupported] = useState(true)
  const [hidden, setHidden] = useState(false)
  const [connectors, setConnectors] = useState<Connector[]>([])
  const [adding, setAdding] = useState(false)
  const [pick, setPick] = useState(EMPTY_PICK)

  const load = useCallback(async () => {
    try {
      const r = await listPrivateGrants(sandboxId)
      setGrants(r.grants)
      setSupported(r.supported)
      setRoutesSupported(r.routes_supported ?? true)
    } catch (e) { if ((e as { status?: number }).status === 501) setHidden(true); else toastRef.current.error(errText(e)) }
  }, [sandboxId])
  useEffect(() => { void load() }, [load])
  useEffect(() => { if (canChange) listConnectors().then(setConnectors).catch(e => toastRef.current.error(errText(e))) }, [canChange])

  if (hidden || grants === null) return null
  const conn = connectors.find(c => c.name === pick.connector)
  const kindOK = pick.kind === 'target' ? supported : supported && routesSupported

  return (
    <section className="panel" aria-label="Private access">
      <div className="ph"><h2>Private access</h2><span className="meta">hosts on your own network, through a connector</span></div>
      <div className="panel-pad">
        {!supported && <p className="muted">Private access is not available on this sandbox's node.</p>}
        {supported && !routesSupported && <p className="muted">Subnet and DNS grants are not available on this sandbox's node.</p>}
        {grants.length === 0 ? <p className="vault-empty">This sandbox reaches no private targets.</p> : (
          <table className="tbl"><tbody>{grants.map(g => (
            <tr key={g.id}>
              <td>
                {reachedAs(g).map(n => <code key={n}>{n}</code>)}
                {g.shadowed && g.shadowed.length > 0 && <div className="muted">Not routed (reserved): {g.shadowed.join(', ')}</div>}
              </td>
              <td>{g.connector}{g.online ? '' : ' (offline)'}</td>
              <td>{KIND_LABEL[g.kind]}{g.source === 'sandbox' ? '' : ` · from ${g.source.replace(/^tag:/, 'tag ')}`}</td>
              {canChange && <td>{g.source === 'sandbox' && <button className="btn ghost" aria-label={`Remove ${g.value}`} onClick={async () => {
                if (!await confirm({ title: `Remove ${reachedAs(g)[0]}?`, body: 'Open connections through it close.', danger: true })) return
                try { await revokePrivate(sandboxId, g.id); await load() } catch (e) { toast.error(errText(e)) }
              }}>Remove</button>}</td>}
            </tr>))}</tbody></table>
        )}
        {canChange && connectors.length === 0 && <p className="vault-empty">No connectors yet. <Link to="/connectors">Create one</Link>.</p>}
        {canChange && connectors.length > 0 && (
          <form className="vault-add" onSubmit={async e => {
            e.preventDefault()
            setAdding(true)
            try { await grantPrivate(sandboxId, pick.connector, pick.kind, pick.value.trim()); setPick(EMPTY_PICK); await load() }
            catch (err) { toast.error(errText(err)) } finally { setAdding(false) }
          }}>
            <select className="sel" aria-label="Connector" value={pick.connector} disabled={!supported} onChange={e => setPick({ ...pick, connector: e.target.value, value: '' })}>
              <option value="">Connector…</option>
              {connectors.map(c => <option key={c.id} value={c.name}>{c.name}</option>)}
            </select>
            <select className="sel" aria-label="Kind" value={pick.kind} disabled={!supported} onChange={e => setPick({ ...pick, kind: e.target.value as GrantKind, value: '' })}>
              {(Object.keys(KIND_LABEL) as GrantKind[]).map(k => <option key={k} value={k}>{KIND_LABEL[k]}</option>)}
            </select>
            {pick.kind === 'target' ? (
              <select className="sel" aria-label="Target" value={pick.value} disabled={!supported || !pick.connector} onChange={e => setPick({ ...pick, value: e.target.value })}>
                <option value="">Target…</option>
                {(conn?.targets ?? []).map(t => <option key={t.name} value={t.name}>{t.name}:{t.port}</option>)}
              </select>
            ) : (
              <input className="inp mono vault-input" aria-label={pick.kind === 'subnet' ? 'Subnet' : 'DNS suffix'} value={pick.value} disabled={!kindOK || !pick.connector}
                placeholder={pick.kind === 'subnet' ? conn?.subnets?.[0] ?? '10.0.0.0/24' : conn?.dns?.suffixes[0] ?? 'corp.internal'}
                onChange={e => setPick({ ...pick, value: e.target.value })} />
            )}
            <button className="btn" type="submit" disabled={!kindOK || !pick.value || adding}>Add</button>
          </form>
        )}
      </div>
    </section>
  )
}

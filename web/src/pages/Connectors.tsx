import { useCallback, useEffect, useRef, useState } from 'react'
import {
  listConnectors, createConnector, revokeConnector, listConnectorKeys, createConnectorKey,
  revokeConnectorKey, listConnectorConnections, listTagGrants, grantTag, revokeTagGrant,
} from '../api'
import type { Connector, ConnectorKey, GrantKind, PrivateConnection, TagGrant } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'
import { useOrgRole } from '../hooks/useOrgRole'
import { usePrivateAccess } from '../hooks/usePrivateAccess'

const NAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

const tagReaches = (g: TagGrant) => {
  if (g.kind === 'target') return `${g.value}.private`
  return g.kind === 'dns' ? `*.${g.value}` : g.value
}

const targetText = (t: { name: string; port: number; host?: string }) =>
  t.host ? `${t.name}:${t.port} (${t.host})` : `${t.name}:${t.port}`

export default function Connectors() {
  const toast = useToast()
  const confirm = useConfirm()
  const role = useOrgRole()
  const canChange = role === 'owner' || role === 'admin'
  const [rows, setRows] = useState<Connector[]>([])
  const [loading, setLoading] = useState(true)
  const [loadErr, setLoadErr] = useState('')
  const on = usePrivateAccess()
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState('')
  const [open, setOpen] = useState<{ id: string; tab: 'keys' | 'log' } | null>(null)

  const refresh = useCallback(async () => {
    try { setRows(await listConnectors()); setLoadErr('') } catch (e) { setLoadErr(errText(e)) } finally { setLoading(false) }
  }, [])
  useEffect(() => {
    if (!on) return
    void refresh()
    const t = setInterval(refresh, 15000)
    return () => clearInterval(t)
  }, [refresh, on])

  const create = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!NAME.test(name)) { toast.error('Use lowercase letters, digits and dashes.'); return }
    setBusy(true)
    try { await createConnector(name); setCreated(name); setName(''); await refresh() } catch (err) { toast.error(errText(err)) } finally { setBusy(false) }
  }

  const remove = async (c: Connector) => {
    if (!await confirm({ title: `Remove ${c.name}?`, body: 'Its instances disconnect, open connections close, and every grant and key for it is removed.', danger: true })) return
    try { await revokeConnector(c.id); setOpen(o => (o?.id === c.id ? null : o)); toast.success(`${c.name} removed`); await refresh() } catch (err) { toast.error(errText(err)) }
  }

  if (on === false) {
    return <div className="wrap"><h1 className="h1">Connectors</h1><p className="vault-empty">This server does not offer private access. An operator turns it on with PRIVATE_INGRESS.</p></div>
  }
  return (
    <div className="wrap">
      <div className="h1row"><h1 className="h1">Connectors</h1></div>
      <p className="muted">A connector runs inside your network and lets the sandboxes you choose reach the targets it lists, as <code>name.private</code>.</p>
      {canChange && (
        <div className="panel vault-panel">
          <form className="vault-add" onSubmit={create}>
            <label className="vault-auto">Connector name<input className="inp mono vault-add-name" aria-label="Connector name" value={name} onChange={e => setName(e.target.value.toLowerCase())} placeholder="prod" /></label>
            <button className="btn" type="submit" disabled={busy || !name}>Create</button>
          </form>
          {created && <pre className="code">pilldrops connector run {created} db=HOST:PORT --subnet 192.0.2.0/24 --dns corp.internal</pre>}
        </div>
      )}
      {loadErr && <p className="lm-error">{loadErr}</p>}
      {loading && <p className="vault-empty">Loading…</p>}
      {!loading && rows.length === 0 && <p className="vault-empty">No connectors yet.</p>}
      {rows.length > 0 && (
        <table className="tbl panel">
          <thead><tr><th>Name</th><th>State</th><th>Reaches</th><th>Last seen</th><th /></tr></thead>
          <tbody>
            {rows.map(c => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td>{c.online ? `online · ${c.instances} instance${c.instances === 1 ? '' : 's'}` : 'offline'}</td>
                <td>{c.targets.length + (c.subnets?.length ?? 0) + (c.dns?.suffixes.length ?? 0) === 0 ? '—' : <>
                  {c.targets.map(t => <div key={t.name}>{targetText(t)}</div>)}
                  {(c.subnets ?? []).map(s => <div key={s}>{s}</div>)}
                  {(c.dns?.suffixes ?? []).map(d => <div key={d}>*.{d}</div>)}
                </>}</td>
                <td>{c.last_seen_at ? new Date(c.last_seen_at * 1000).toLocaleString() : 'never'}</td>
                <td>
                  <button className="btn ghost" aria-label={`Log for ${c.name}`} onClick={() => setOpen({ id: c.id, tab: 'log' })}>Log</button>
                  {canChange && <button className="btn ghost" aria-label={`Keys for ${c.name}`} onClick={() => setOpen({ id: c.id, tab: 'keys' })}>Keys</button>}
                  {canChange && <button className="btn ghost" aria-label={`Remove ${c.name}`} onClick={() => remove(c)}>Remove</button>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {rows.length > 0 && <TagGrantsPanel connectors={rows} canChange={canChange} />}
      {open?.tab === 'keys' && <KeysPanel key={open.id} id={open.id} onDone={() => setOpen(null)} />}
      {open?.tab === 'log' && <LogPanel key={open.id} id={open.id} onDone={() => setOpen(null)} />}
    </div>
  )
}

const EMPTY_TAG_PICK = { tag: '', connector: '', kind: 'target' as GrantKind, value: '' }

function TagGrantsPanel({ connectors, canChange }: { connectors: Connector[]; canChange: boolean }) {
  const toast = useToast()
  const toastRef = useRef(toast)
  toastRef.current = toast
  const confirm = useConfirm()
  const [grants, setGrants] = useState<TagGrant[]>([])
  const [loadErr, setLoadErr] = useState('')
  const [pick, setPick] = useState(EMPTY_TAG_PICK)
  const [busy, setBusy] = useState(false)
  const load = useCallback(() => { listTagGrants().then(g => { setGrants(g); setLoadErr('') }).catch(e => setLoadErr(errText(e))) }, [])
  useEffect(load, [load])
  const conn = connectors.find(c => c.name === pick.connector)
  return (
    <section className="panel" aria-label="Tag grants">
      <div className="ph"><h2>Tag grants</h2><span className="meta">every sandbox in the tag, including ones that join later</span></div>
      <div className="panel-pad">
      {loadErr && <p className="lm-error">{loadErr}</p>}
      {!loadErr && grants.length === 0 && <p className="vault-empty">No tag grants.</p>}
      {!loadErr && grants.length > 0 && (
        <table className="tbl"><thead><tr><th>Tag</th><th>Connector</th><th>Reaches</th><th /></tr></thead>
          <tbody>{grants.map(g => (
            <tr key={g.id}>
              <td>{g.tag}</td>
              <td>{g.connector}</td>
              <td><code>{tagReaches(g)}</code>
                {g.shadowed && g.shadowed.length > 0 && <div className="muted">Not routed (reserved): {g.shadowed.join(', ')}</div>}</td>
              <td>{canChange && <button className="btn ghost" aria-label={`Remove ${g.value} from ${g.tag} via ${g.connector}`} onClick={async () => {
                if (!await confirm({ title: `Remove ${g.value} from tag ${g.tag}?`, body: 'Sandboxes in the tag lose it, and open connections through it close.', danger: true })) return
                try { await revokeTagGrant(g.id); load() } catch (e) { toast.error(errText(e)) }
              }}>Remove</button>}</td>
            </tr>))}</tbody></table>
      )}
      {canChange && (
        <form className="vault-add" onSubmit={async e => {
          e.preventDefault()
          setBusy(true)
          try {
            const r = await grantTag(pick.tag.trim(), pick.connector, pick.kind, pick.value.trim())
            toast.success(r.unsupported.length === 0 ? `Granted to ${r.sandboxes} sandbox${r.sandboxes === 1 ? '' : 'es'}`
              : `Granted; ${r.unsupported.length} of ${r.sandboxes} run on a node that cannot use it yet`)
            setPick(EMPTY_TAG_PICK)
            load()
          } catch (err) { toast.error(errText(err)) } finally { setBusy(false) }
        }}>
          <label className="vault-auto">Tag<input className="inp mono vault-add-name" aria-label="Tag" value={pick.tag} placeholder="backend" onChange={e => setPick({ ...pick, tag: e.target.value.toLowerCase() })} /></label>
          <select className="sel" aria-label="Tag connector" value={pick.connector} onChange={e => setPick({ ...pick, connector: e.target.value, value: '' })}>
            <option value="">Connector…</option>
            {connectors.map(c => <option key={c.id} value={c.name}>{c.name}</option>)}
          </select>
          <select className="sel" aria-label="Tag kind" value={pick.kind} onChange={e => setPick({ ...pick, kind: e.target.value as GrantKind, value: '' })}>
            <option value="target">Target</option><option value="subnet">Subnet</option><option value="dns">DNS</option>
          </select>
          {pick.kind === 'target' ? (
            <select className="sel" aria-label="Tag target" value={pick.value} disabled={!pick.connector} onChange={e => setPick({ ...pick, value: e.target.value })}>
              <option value="">Target…</option>
              {(conn?.targets ?? []).map(t => <option key={t.name} value={t.name}>{t.name}:{t.port}</option>)}
            </select>
          ) : (
            <input className="inp mono vault-input" aria-label="Tag value" value={pick.value} disabled={!pick.connector}
              placeholder={pick.kind === 'subnet' ? conn?.subnets?.[0] ?? '10.0.0.0/24' : conn?.dns?.suffixes[0] ?? 'corp.internal'}
              onChange={e => setPick({ ...pick, value: e.target.value })} />
          )}
          <button className="btn" type="submit" disabled={busy || !pick.tag || !pick.connector || !pick.value}>Grant</button>
        </form>
      )}
      </div>
    </section>
  )
}

function KeysPanel({ id, onDone }: { id: string; onDone: () => void }) {
  const toast = useToast()
  const toastRef = useRef(toast)
  toastRef.current = toast
  const confirm = useConfirm()
  const [keys, setKeys] = useState<ConnectorKey[]>([])
  const [fresh, setFresh] = useState('')
  const load = useCallback(() => { listConnectorKeys(id).then(setKeys).catch(e => toastRef.current.error(errText(e))) }, [id])
  useEffect(load, [load])
  return (
    <div className="panel">
      <h2>Keys</h2>
      <button className="btn ghost" onClick={onDone}>Done</button>
      <p className="muted">A key lets a server run this connector without your login: <code>PILLDROPS_CONNECTOR_KEY=… pilldrops connector run …</code></p>
      <button className="btn" onClick={async () => { try { const k = await createConnectorKey(id); setFresh(k.key); load() } catch (e) { toast.error(errText(e)) } }}>Create key</button>
      {fresh && <><pre className="code">{fresh}</pre><p className="muted">This key is shown once. Store it now.</p></>}
      {keys.length === 0 ? <p className="vault-empty">No keys.</p> : (
        <table className="tbl"><tbody>{keys.map(k => (
          <tr key={k.id}><td>{k.id}</td><td>{k.last_used_at ? `used ${new Date(k.last_used_at * 1000).toLocaleString()}` : 'never used'}</td>
            <td><button className="btn ghost" onClick={async () => {
              if (!await confirm({ title: `Revoke ${k.id}?`, body: 'Instances using it are disconnected and cannot reconnect.', danger: true })) return
              try { await revokeConnectorKey(id, k.id); load() } catch (e) { toast.error(errText(e)) }
            }}>Revoke</button></td></tr>))}</tbody></table>
      )}
    </div>
  )
}

function LogPanel({ id, onDone }: { id: string; onDone: () => void }) {
  const toast = useToast()
  const toastRef = useRef(toast)
  toastRef.current = toast
  const [rows, setRows] = useState<PrivateConnection[] | null>(null)
  useEffect(() => { listConnectorConnections(id).then(setRows).catch(e => { toastRef.current.error(errText(e)); setRows([]) }) }, [id])
  if (rows === null) return <p className="vault-empty">Loading…</p>
  return (
    <div className="panel">
      <h2>Connections</h2>
      <button className="btn ghost" onClick={onDone}>Done</button>
      {rows.length === 0 ? <p className="vault-empty">No connections yet.</p> : (
        <table className="tbl"><thead><tr><th>Sandbox</th><th>Target</th><th>Started</th><th>Duration</th><th>Bytes</th><th>Result</th></tr></thead>
          <tbody>{rows.map(r => (
            <tr key={r.id}><td>{r.sandbox_id}</td><td>{r.target}{r.dest ? ` → ${r.dest}` : ''}</td><td>{new Date(r.started_at * 1000).toLocaleString()}</td>
              <td>{r.ended_at ? `${r.ended_at - r.started_at}s` : 'open'}</td><td>{r.bytes_in} in / {r.bytes_out} out</td><td>{r.result.replace('_', ' ')}</td></tr>
          ))}</tbody></table>
      )}
    </div>
  )
}

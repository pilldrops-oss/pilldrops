import { useCallback, useEffect, useRef, useState } from 'react'
import {
  listConnectors, createConnector, revokeConnector, listConnectorKeys, createConnectorKey,
  revokeConnectorKey, listConnectorConnections, getCapabilities,
} from '../api'
import type { Connector, ConnectorKey, PrivateConnection } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'
import { useOrgRole } from '../hooks/useOrgRole'

const NAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

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
  const [on, setOn] = useState<boolean | null>(null)
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState('')
  const [open, setOpen] = useState<{ id: string; tab: 'keys' | 'log' } | null>(null)

  useEffect(() => { getCapabilities().then(c => setOn(!!c.private_access)).catch(() => setOn(true)) }, [])

  const refresh = useCallback(async () => {
    try { setRows(await listConnectors()); setLoadErr('') } catch (e) { setLoadErr(errText(e)) } finally { setLoading(false) }
  }, [])
  useEffect(() => { void refresh(); const t = setInterval(refresh, 15000); return () => clearInterval(t) }, [refresh])

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
            <label>Connector name<input aria-label="Connector name" value={name} onChange={e => setName(e.target.value.toLowerCase())} placeholder="prod" /></label>
            <button className="btn" type="submit" disabled={busy || !name}>Create</button>
          </form>
          {created && <pre className="code">pilldrops connector run {created} db=HOST:PORT</pre>}
        </div>
      )}
      {loadErr && <p className="lm-error">{loadErr}</p>}
      {loading && <p className="vault-empty">Loading…</p>}
      {!loading && rows.length === 0 && <p className="vault-empty">No connectors yet.</p>}
      {rows.length > 0 && (
        <table className="tbl panel">
          <thead><tr><th>Name</th><th>State</th><th>Targets</th><th>Last seen</th><th /></tr></thead>
          <tbody>
            {rows.map(c => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td>{c.online ? `online · ${c.instances} instance${c.instances === 1 ? '' : 's'}` : 'offline'}</td>
                <td>{c.targets.length === 0 ? '—' : c.targets.map(t => <div key={t.name}>{targetText(t)}</div>)}</td>
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
      {open?.tab === 'keys' && <KeysPanel key={open.id} id={open.id} onDone={() => setOpen(null)} />}
      {open?.tab === 'log' && <LogPanel key={open.id} id={open.id} onDone={() => setOpen(null)} />}
    </div>
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
              if (!await confirm({ title: `Revoke ${k.id}?`, body: 'Instances using it cannot reconnect.', danger: true })) return
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
            <tr key={r.id}><td>{r.sandbox_id}</td><td>{r.target}</td><td>{new Date(r.started_at * 1000).toLocaleString()}</td>
              <td>{r.ended_at ? `${r.ended_at - r.started_at}s` : 'open'}</td><td>{r.bytes_in} in / {r.bytes_out} out</td><td>{r.result.replace('_', ' ')}</td></tr>
          ))}</tbody></table>
      )}
    </div>
  )
}

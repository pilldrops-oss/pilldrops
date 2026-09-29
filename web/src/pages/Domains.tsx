import { useCallback, useEffect, useState } from 'react'
import { listDomains, addDomain, removeDomain } from '../api'
import type { CustomDomain } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'

// Domains — your own hostname in front of a public sandbox. The row is stored
// immediately and stays `pending` until the platform resolves the DNS record
// itself, so the record shown here is the whole job: nothing routes, and no
// certificate is issued, before it exists.

export default function Domains() {
  const toast = useToast()
  const confirm = useConfirm()

  const [rows, setRows] = useState<CustomDomain[]>([])
  const [loading, setLoading] = useState(true)
  const [loadErr, setLoadErr] = useState('')
  const [host, setHost] = useState('')
  const [sandbox, setSandbox] = useState('')
  const [port, setPort] = useState('')
  const [busy, setBusy] = useState(false)

  const refresh = useCallback(async () => {
    try {
      setRows(await listDomains())
      setLoadErr('')
    } catch (e) {
      setLoadErr(errText(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { refresh() }, [refresh])

  // A pending row is waiting on DNS, which the verifier re-reads every 30 s.
  useEffect(() => {
    if (!rows.some(d => d.status === 'pending')) return
    const t = setInterval(refresh, 15000)
    return () => clearInterval(t)
  }, [rows, refresh])

  const add = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      const d = await addDomain(host.trim(), sandbox.trim(), port ? Number(port) : undefined)
      setHost(''); setSandbox(''); setPort('')
      await refresh()
      toast.success(`Now create the DNS record: ${d.record}`)
    } catch (err) {
      toast.error(errText(err))
    } finally {
      setBusy(false)
    }
  }

  const remove = async (d: CustomDomain) => {
    if (!await confirm({ title: `Remove ${d.host}?`, body: 'The host stops routing immediately.' })) return
    try {
      await removeDomain(d.host)
      await refresh()
    } catch (err) {
      toast.error(errText(err))
    }
  }

  return (
    <div className="wrap">
      <h1>Domains</h1>
      <p className="muted">
        Put your own hostname in front of a <strong>public</strong> sandbox. Create the CNAME
        record we show, and the host starts serving within a minute. A wildcard
        (<code>*.vms.example.com</code>, no sandbox) addresses every sandbox of your org by label.
      </p>

      <form className="row" onSubmit={add}>
        <input value={host} onChange={e => setHost(e.target.value)}
               placeholder="app.example.com" aria-label="host" required />
        <input value={sandbox} onChange={e => setSandbox(e.target.value)}
               placeholder="sandbox id (blank for a wildcard)" aria-label="sandbox" />
        <input value={port} onChange={e => setPort(e.target.value)}
               placeholder="port (8000)" aria-label="port" inputMode="numeric" />
        <button type="submit" disabled={busy || !host.trim()}>Add domain</button>
      </form>

      {loading && <p className="muted">Loading…</p>}
      {loadErr && <p className="err">{loadErr}</p>}

      {!loading && rows.length === 0 && <p className="muted">No domains yet.</p>}

      {rows.length > 0 && (
        <table className="tbl">
          <thead>
            <tr><th>Host</th><th>Sandbox</th><th>Status</th><th>DNS record</th><th /></tr>
          </thead>
          <tbody>
            {rows.map(d => (
              <tr key={d.host}>
                <td>{d.host}</td>
                <td>{d.wildcard ? '(any, by label)' : `${d.sandbox_id}:${d.port}`}</td>
                <td>
                  <span className={d.status === 'active' ? 'badge ok' : 'badge warn'}>{d.status}</span>
                  {d.error && <div className="muted small">{d.error}</div>}
                </td>
                <td><code>{d.record}</code></td>
                <td><button onClick={() => remove(d)}>Remove</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

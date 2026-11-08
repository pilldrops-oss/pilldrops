import { useCallback, useEffect, useState } from 'react'
import { listDomains, addDomain, removeDomain, getCapabilities } from '../api'
import type { CustomDomain } from '../types'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/ConfirmModal'
import { errText } from '../lib/errText'
import { invalidHostnameReason } from '../lib/hostname'

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
  const [hostErr, setHostErr] = useState('')
  // null = not known yet; false disables the form up front instead of
  // letting the user fill it in and hit the 501 the server would return.
  const [customDomainsOn, setCustomDomainsOn] = useState<boolean | null>(null)

  useEffect(() => {
    getCapabilities().then(c => setCustomDomainsOn(c.custom_domains)).catch(() => setCustomDomainsOn(null))
  }, [])

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
    const reason = invalidHostnameReason(host)
    if (reason) {
      setHostErr(reason)
      return
    }
    setHostErr('')
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
      <div className="h1row">
        <div>
          <h1 className="h1">Domains</h1>
          <p>
            Put your own hostname in front of a <b>public</b> sandbox. Create the CNAME
            record we show, and the host starts serving within a minute. A wildcard
            (<code>*.vms.example.com</code>, no sandbox) addresses every sandbox of your org by label.
          </p>
        </div>
      </div>

      {customDomainsOn === false && (
        <p className="lm-error">
          Custom domains are off on this server: it has no PORTAL_SANDBOX_DOMAIN set, so there is no
          CNAME target to point at. Adding one below would only fail.
        </p>
      )}

      <div className="panel vault-panel">
        <form className="vault-add" onSubmit={add}>
          <input className="pa-input mono vault-input" value={host} onChange={e => { setHost(e.target.value); setHostErr('') }}
                 placeholder="app.example.com" aria-label="host" required disabled={customDomainsOn === false} />
          <input className="pa-input mono vault-input" value={sandbox} onChange={e => setSandbox(e.target.value)}
                 placeholder="sandbox id (blank for a wildcard)" aria-label="sandbox" disabled={customDomainsOn === false} />
          <input className="pa-input mono vault-add-name" value={port} onChange={e => setPort(e.target.value)}
                 placeholder="port (8000)" aria-label="port" inputMode="numeric" disabled={customDomainsOn === false} />
          <button type="submit" className="btn primary" disabled={busy || !host.trim() || customDomainsOn === false}>
            Add domain
          </button>
        </form>
        {hostErr && <p className="lm-error">{hostErr}</p>}
        {loadErr && <p className="lm-error">{loadErr}</p>}
      </div>

      {loading && <p className="vault-empty">Loading…</p>}
      {!loading && rows.length === 0 && <p className="vault-empty">No domains yet.</p>}

      {rows.length > 0 && (
        <table className="tbl panel">
          <thead>
            <tr><th>Host</th><th>Sandbox</th><th>Status</th><th>DNS record</th><th /></tr>
          </thead>
          <tbody>
            {rows.map(d => (
              <tr key={d.host}>
                <td>{d.host}</td>
                <td>{d.wildcard ? '(any, by label)' : `${d.sandbox_id}:${d.port}`}</td>
                <td>
                  <span className={d.status === 'active' ? 'pill run' : 'pill pau'}><span className="pd" />{d.status}</span>
                  {d.error && <div className="lm-error">{d.error}</div>}
                  {d.legacy_target && (
                    <div className="lm-error">
                      Update your DNS record to <code>{d.record}</code>. The old one keeps working only while this host gets traffic.
                    </div>
                  )}
                </td>
                <td><code>{d.record}</code></td>
                <td><button className="btn ghost" onClick={() => remove(d)}>Remove</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

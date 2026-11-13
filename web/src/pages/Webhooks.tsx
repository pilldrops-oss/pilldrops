import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { addWebhook, deleteWebhook, listWebhookDeliveries, listWebhooks, setWebhookEnabled, testWebhook } from '../api'
import type { Webhook, WebhookDelivery, WebhookTest } from '../types'
import { useConfirm } from '../components/ConfirmModal'
import { CopyCli } from '../components/launch/panels'
import { errText } from '../lib/errText'
import { timeAgo } from '../lib/launchMemory'
import { WEBHOOK_SNIPPETS, webhookSnippets, type WebhookSnippetKey } from '../lib/webhookSnippets'

const PRESETS = [
  { label: 'A sandbox starts or ends', events: ['sandbox.created', 'sandbox.ended'] },
  { label: 'Something goes wrong', events: ['sandbox.crashed', 'sandbox.out_of_memory', 'sandbox.unreachable'] },
]

const EXAMPLE_URL = 'https://example.com/hooks/pilldrops'

export default function Webhooks() {
  const confirm = useConfirm()
  const [hooks, setHooks] = useState<Webhook[] | null>(null)
  const [deliverable, setDeliverable] = useState<string[]>([])
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([])
  const [err, setErr] = useState('')
  const [url, setURL] = useState('')
  const [events, setEvents] = useState<string[]>(PRESETS[0].events)
  const [busy, setBusy] = useState(false)
  const [secret, setSecret] = useState('')
  const [tests, setTests] = useState<Record<string, WebhookTest | 'sending'>>({})
  const [snippet, setSnippet] = useState<WebhookSnippetKey>('curl')

  const load = useCallback(async () => {
    try {
      const [list, sent] = await Promise.all([listWebhooks(), listWebhookDeliveries()])
      setHooks(list.webhooks ?? [])
      setDeliverable(list.deliverable_events)
      setDeliveries(sent.deliveries ?? [])
    } catch (e) {
      setErr(errText(e))
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const add = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    try {
      const added = await addWebhook(url.trim(), events)
      setSecret(added.secret)
      setURL('')
      await load()
    } catch (e) {
      setErr(errText(e))
    } finally {
      setBusy(false)
    }
  }

  const sendTest = async (hookID: string) => {
    setTests(t => ({ ...t, [hookID]: 'sending' }))
    try {
      const sent = await testWebhook(hookID)
      setTests(t => ({ ...t, [hookID]: sent }))
    } catch (e) {
      setTests(t => ({ ...t, [hookID]: { ok: false, status: 0, error: errText(e) } }))
    }
    await load()
  }

  const remove = async (hook: Webhook) => {
    const yes = await confirm({
      title: 'Delete this webhook?',
      body: `${hook.url} will stop receiving events. Its signing secret cannot be recovered.`,
      confirmLabel: 'Delete',
      danger: true,
    })
    if (!yes) return
    try {
      await deleteWebhook(hook.id)
      await load()
    } catch (e) {
      setErr(errText(e))
    }
  }

  const setEnabled = async (hook: Webhook, enabled: boolean) => {
    try {
      await setWebhookEnabled(hook.id, enabled)
      await load()
    } catch (e) {
      setErr(errText(e))
    }
  }

  const toggle = (event: string) =>
    setEvents(picked => (picked.includes(event) ? picked.filter(e => e !== event) : [...picked, event]))

  const snippets = webhookSnippets({
    origin: window.location.origin,
    url: url.trim() || hooks?.[0]?.url || EXAMPLE_URL,
    events: events.length > 0 ? events : PRESETS[0].events,
    hookID: hooks?.[0]?.id,
  })

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Webhooks</h1>
          <p>
            Hear about every sandbox in your org without asking: we send a signed request to your
            URL when one starts, ends, pauses or runs into trouble.
          </p>
        </div>
      </div>

      {err && <p className="lm-error" role="alert">{err}</p>}

      {secret && (
        <div className="callout success" role="status">
          <p>
            Copy this signing secret now: it is shown once. Your endpoint uses it to check that a
            request came from us.
          </p>
          <CopyCli snippet={secret} />
          <button className="btn small" onClick={() => setSecret('')}>I have saved it</button>
        </div>
      )}

      {hooks === null && !err && <p className="vault-empty">Loading…</p>}
      {hooks?.length === 0 && <p className="vault-empty">No webhooks yet. Add one below.</p>}
      {hooks && hooks.length > 0 && (
        <table className="tbl panel">
          <thead>
            <tr><th>URL</th><th>Events</th><th>Status</th><th /></tr>
          </thead>
          <tbody>
            {hooks.map(hook => {
              const test = tests[hook.id]
              return (
                <tr key={hook.id}>
                  <td>
                    <code>{hook.url}</code>
                    <div className="hint"><code>{hook.id}</code></div>
                  </td>
                  <td>{hook.events.join(', ')}</td>
                  <td>
                    <HookStatus hook={hook} />
                    {test && test !== 'sending' && (
                      test.ok
                        ? <div className="hint" role="status">Delivered. Your endpoint answered {test.status}.</div>
                        : <div className="lm-error" role="status">Not delivered. {test.error ?? `Your endpoint answered ${test.status}.`}</div>
                    )}
                  </td>
                  <td>
                    <button className="btn ghost" onClick={() => void sendTest(hook.id)} disabled={test === 'sending'}>
                      {test === 'sending' ? 'Sending…' : 'Send test'}
                    </button>
                    <button className="btn ghost" onClick={() => void setEnabled(hook, !hook.enabled)}>
                      {hook.enabled ? 'Pause' : 'Resume'}
                    </button>
                    <button className="btn ghost" onClick={() => void remove(hook)}>Delete</button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}

      <div className="panel vault-panel">
        <form onSubmit={e => void add(e)}>
          <label className="ws-field">
            <span>Your URL</span>
            <input type="url" value={url} onChange={e => setURL(e.target.value)} placeholder={EXAMPLE_URL} required />
            <small>Must be https and reachable from the internet.</small>
          </label>
          <div className="vault-add">
            {[...PRESETS, { label: 'Everything', events: deliverable }].map(p => (
              <button key={p.label} type="button" className="btn small" onClick={() => setEvents(p.events)}>
                {p.label}
              </button>
            ))}
          </div>
          <div className="vault-add">
            {deliverable.map(event => (
              <label key={event} className="hint">
                <input type="checkbox" checked={events.includes(event)} onChange={() => toggle(event)} /> {event}
              </label>
            ))}
          </div>
          <button className="btn primary" type="submit" disabled={busy || events.length === 0}>
            {busy ? 'Adding…' : 'Add webhook'}
          </button>
        </form>
      </div>

      <div className="panel vault-panel">
        <p className="vault-empty">The same steps work from a terminal, and your endpoint checks each request like this:</p>
        <div className="vault-add">
          {WEBHOOK_SNIPPETS.map(s => (
            <button key={s.key} className={snippet === s.key ? 'btn small primary' : 'btn small'} onClick={() => setSnippet(s.key)}>
              {s.label}
            </button>
          ))}
        </div>
        <CopyCli snippet={snippets[snippet]} />
      </div>

      {deliveries.length > 0 && (
        <table className="tbl panel">
          <thead>
            <tr><th>Recent deliveries</th><th>Sandbox</th><th>Answer</th><th /></tr>
          </thead>
          <tbody>
            {deliveries.map(d => (
              <tr key={d.id}>
                <td>{d.event_type}</td>
                <td><Link to={`/sandbox/${d.sandbox_id}`}>{d.sandbox_id}</Link></td>
                <td>{deliveryAnswer(d)}</td>
                <td className="hint">{d.attempts} tries · {timeAgo(Date.parse(d.created_at))}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

function deliveryAnswer(d: WebhookDelivery) {
  if (d.state === 'sent') return `answered ${d.last_status}`
  const why = d.last_error || (d.last_status ? `answered ${d.last_status}` : '')
  if (d.state === 'pending') return why ? `retrying · ${why}` : 'waiting to send'
  return why
}

function HookStatus({ hook }: { hook: Webhook }) {
  if (!hook.enabled) {
    return <span className="pill pau"><span className="pd" />off{hook.disabled_reason ? `: ${hook.disabled_reason}` : ''}</span>
  }
  if (hook.consecutive_failures > 0) {
    return (
      <span className="pill pau">
        <span className="pd" />failing · {hook.consecutive_failures} in a row{hook.last_status ? ` · last ${hook.last_status}` : ''}
      </span>
    )
  }
  // The server sends a zero time, not an empty one, before the first delivery.
  if (!hook.last_status || !hook.last_at) return <span className="hint">No deliveries yet</span>
  return <span className="pill run"><span className="pd" />last delivery {hook.last_status} · {timeAgo(Date.parse(hook.last_at))}</span>
}

import { useState, useEffect, useRef, useCallback } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { previewUrl,
  getFunction, getFunctionLogs, getFunctionCode, getFunctionMetrics,
  invokeFunction, updateFunction, deleteFunction,
} from '../api'
import type { FunctionInfo, InvokeResult, FunctionMetrics, InvokeOptions } from '../types'
import CodeEditor from '../components/LazyCodeEditor'
import { type EditorLang, langForTemplate } from '../components/editorLang'
import { errText } from '../lib/errText'

type Tab = 'overview' | 'invoke' | 'logs' | 'metrics' | 'code' | 'settings'

function relTime(unixSecs: number): string {
  if (!unixSecs) return '—'
  const diff = Math.max(0, Date.now() / 1000 - unixSecs)
  if (diff < 60) return `${Math.floor(diff)}s ago`
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return new Date(unixSecs * 1000).toLocaleDateString()
}

function statusOf(fn: FunctionInfo, m: FunctionMetrics | null): { label: string; cls: string } {
  if (!fn.sandbox_id) return { label: 'Cold', cls: 'pau' }
  if (m && m.state === 'paused') return { label: 'Paused', cls: 'pau' }
  return { label: 'Warm', cls: 'run' }
}

function OverviewTab({ fn, metrics, onGoInvoke }: { fn: FunctionInfo; metrics: FunctionMetrics | null; onGoInvoke: () => void }) {
  const [copied, setCopied] = useState(false)
  const url = `${window.location.origin}/v1/functions/${fn.name}/invoke`
  const ratio = fn.invokes > 0 ? Math.round((fn.cold_starts / fn.invokes) * 100) : 0
  const key = window.__SANDBOXD__?.key ?? ''
  const curlCmd = `curl -H "Authorization: Bearer ${key}" ${url}`
  const fnPreviewUrl = fn.sandbox_id ? previewUrl(fn.sandbox_id, fn.port) : ''

  function copy() {
    navigator.clipboard?.writeText(curlCmd).then(() => {
      setCopied(true); setTimeout(() => setCopied(false), 1500)
    }).catch(() => {})
  }

  const perHour = ((fn.vcpu || 0) * 0.0000140 + ((fn.memory_mb || 0) / 1024) * 0.0000045) * 3600
  const cards = [
    { l: 'Resources', v: `${fn.vcpu || '?'} vCPU · ${(fn.memory_mb || 0) >= 1024 ? `${(fn.memory_mb || 0) / 1024} GB` : `${fn.memory_mb || 0} MB`}` },
    { l: 'Replicas (live / max)', v: `${fn.replicas ?? 0} / ${fn.max_replicas || 3}` },
    { l: 'Cost while warm', v: `$${(perHour * Math.max(1, fn.replicas ?? 0)).toFixed(4)}/hr` },
    { l: 'Invokes', v: String(fn.invokes) },
    { l: 'Cold starts', v: String(fn.cold_starts) },
    { l: 'Cold-start ratio', v: `${ratio}%` },
    { l: 'Last invoked', v: relTime(fn.last_invoked_at) },
    { l: 'vCPU-seconds', v: (metrics?.vcpu_seconds ?? 0).toFixed(2) },
    { l: 'RAM GB-s', v: (metrics?.ram_gb_seconds ?? 0).toFixed(3) },
  ]

  return (
    <div className="tabpane">
      <div className="urlbar">
        <div className="field-label">Invoke URL <span style={{ fontWeight: 400, color: 'var(--sub)' }}>· authenticated API (needs a Bearer token)</span></div>
        <div className="urlbar-row">
          <code className="urlbar-code">{url}</code>
          <button className="btn" onClick={copy}>{copied ? 'Copied ✓' : 'Copy curl'}</button>
        </div>
        <div style={{ fontSize: 12, color: 'var(--sub)', marginTop: 6 }}>
          Opening this in a browser returns <code>missing bearer token</code> — it's an API. Use “Copy curl” for a ready-to-run authenticated request.
        </div>
      </div>
      {fnPreviewUrl && (
        <div className="urlbar" style={{ marginTop: 12 }}>
          <div className="field-label">Preview URL <span style={{ fontWeight: 400, color: 'var(--sub)' }}>· open in a browser (no auth) — for functions that serve a UI</span></div>
          <div className="urlbar-row">
            <code className="urlbar-code">{fnPreviewUrl}</code>
            <a className="btn primary" href={fnPreviewUrl} target="_blank" rel="noopener noreferrer">Open app ↗</a>
          </div>
        </div>
      )}
      <div className="metrics six">
        {cards.map(c => (
          <div className="metric" key={c.l}>
            <div className="l">{c.l}</div>
            <div className="v">{c.v}</div>
          </div>
        ))}
      </div>
      <div><button className="btn primary" onClick={onGoInvoke}>Invoke this function →</button></div>
    </div>
  )
}

function InvokeTab({ name, onInvoked }: { name: string; onInvoked?: () => void }) {
  const [method, setMethod] = useState('POST')
  const [path, setPath] = useState('')
  const [headers, setHeaders] = useState<Array<{ k: string; v: string }>>([{ k: '', v: '' }])
  const [body, setBody] = useState('{\n  "name": "world"\n}')
  const [res, setRes] = useState<InvokeResult | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function run() {
    setBusy(true); setErr(null)
    try {
      const hdrs: Record<string, string> = {}
      headers.forEach(h => { if (h.k.trim()) hdrs[h.k.trim()] = h.v })
      const opts: InvokeOptions = { method, path, headers: hdrs, body }
      setRes(await invokeFunction(name, opts))
      onInvoked?.()
    } catch (e) {
      setErr(String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="tabpane">
      <div className="formrow">
        <select className="sel" style={{ width: 110, fontFamily: 'var(--mono)' }} value={method} onChange={e => setMethod(e.target.value)}>
          {['POST', 'GET', 'PUT', 'DELETE', 'PATCH'].map(m => <option key={m} value={m}>{m}</option>)}
        </select>
        <input className="inp mono" placeholder="/optional/path" value={path} onChange={e => setPath(e.target.value)} />
      </div>
      <div className="field">
        <span className="field-label">Headers</span>
        {headers.map((h, i) => (
          <div className="kvrow no-eye" key={i}>
            <input className="inp mono" placeholder="Header" value={h.k}
              onChange={e => setHeaders(hs => hs.map((x, j) => j === i ? { ...x, k: e.target.value } : x))} />
            <input className="inp mono" placeholder="value" value={h.v}
              onChange={e => setHeaders(hs => hs.map((x, j) => j === i ? { ...x, v: e.target.value } : x))} />
            <button className="iconbtn sm" onClick={() => setHeaders(hs => hs.filter((_, j) => j !== i))} aria-label="Remove header">✕</button>
          </div>
        ))}
        <button className="btn ghost" onClick={() => setHeaders(hs => [...hs, { k: '', v: '' }])}>+ Add header</button>
      </div>
      <div className="field">
        <span className="field-label">Request body</span>
        <textarea className="inp mono codearea" value={body} onChange={e => setBody(e.target.value)} spellCheck={false} />
      </div>
      <div className="savebar">
        <button className="btn primary" disabled={busy} onClick={run}>{busy ? 'Sending…' : 'Send request'}</button>
      </div>
      {err && <div className="savemsg err">{err}</div>}
      {res && (
        <div className="panel resp">
          <div className="resp-meta">
            <span>status <b className={res.status < 400 ? 'ok' : 'bad'}>{res.status}</b></span>
            <span>{res.duration_ms} ms</span>
            <span className={`pill ${res.cold_start ? 'pau' : 'run'}`}><span className="pd" />{res.cold_start ? 'cold start' : 'warm'}</span>
          </div>
          <pre className="resp-body">{res.body}</pre>
        </div>
      )}
    </div>
  )
}

function LogsTab({ name }: { name: string }) {
  const [lines, setLines] = useState<string[]>([])
  const [note, setNote] = useState('')
  const [autoScroll, setAutoScroll] = useState(true)
  const bottomRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let alive = true
    const load = () => getFunctionLogs(name).then(r => {
      if (!alive) return
      setLines(r.lines ?? []); setNote(r.note ?? '')
    }).catch(() => {})
    load()
    const t = setInterval(load, 3000)
    return () => { alive = false; clearInterval(t) }
  }, [name])

  useEffect(() => { if (autoScroll) bottomRef.current?.scrollIntoView() }, [lines, autoScroll])

  return (
    <div className="tabpane logpane">
      <div className="logbar">
        <span className="hint">{lines.length} lines</span>
        <label className="chk"><input type="checkbox" checked={autoScroll} onChange={e => setAutoScroll(e.target.checked)} /> auto-scroll</label>
        <button className="btn" onClick={() => setLines([])}>Clear</button>
      </div>
      <div className="log-feed">
        {lines.length === 0
          ? <div className="empty">{note || 'No output yet'}<p>Invoke the function to see handler logs stream here.</p></div>
          : lines.map((l, i) => <div className="logrow" key={i}>{l}</div>)}
        <div ref={bottomRef} />
      </div>
    </div>
  )
}

function Sparkline({ points }: { points: number[] }) {
  const W = 600, H = 90
  if (points.length < 2) return <div className="empty" style={{ padding: '30px 0' }}>Gathering data…</div>
  const max = Math.max(...points, 1)
  const step = W / (points.length - 1)
  const line = points.map((v, i) => `${i * step},${H - (v / max) * (H - 10) - 5}`).join(' ')
  const area = `${line} ${W},${H} 0,${H}`
  return (
    <svg width="100%" height={H} viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none">
      <defs>
        <linearGradient id="fnspark" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="var(--green)" stopOpacity=".16" />
          <stop offset="1" stopColor="var(--green)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={area} fill="url(#fnspark)" />
      <polyline points={line} fill="none" stroke="var(--green)" strokeWidth="2" />
    </svg>
  )
}

function MetricsTab({ name }: { name: string }) {
  const [m, setM] = useState<FunctionMetrics | null>(null)
  const [hist, setHist] = useState<number[]>([])

  useEffect(() => {
    let alive = true
    const load = () => getFunctionMetrics(name).then(r => {
      if (!alive) return
      setM(r); setHist(h => [...h, r.invokes].slice(-40))
    }).catch(() => {})
    load()
    const t = setInterval(load, 3000)
    return () => { alive = false; clearInterval(t) }
  }, [name])

  const ratio = m && m.invokes > 0 ? Math.round((m.cold_starts / m.invokes) * 100) : 0
  const cards = [
    { l: 'Invokes', v: String(m?.invokes ?? 0) },
    { l: 'Cold starts', v: String(m?.cold_starts ?? 0) },
    { l: 'Cold-start ratio', v: `${ratio}%` },
    { l: 'vCPU-seconds', v: (m?.vcpu_seconds ?? 0).toFixed(2) },
    { l: 'RAM GB-s', v: (m?.ram_gb_seconds ?? 0).toFixed(3) },
    { l: 'Uptime', v: `${Math.round(m?.uptime_seconds ?? 0)}s` },
  ]

  return (
    <div className="tabpane">
      <div className="panel">
        <div className="ph"><h2>Invocations</h2><span className="meta">live · 3s</span></div>
        <div className="chartwrap"><Sparkline points={hist} /></div>
      </div>
      <div className="metrics six">
        {cards.map(c => <div className="metric" key={c.l}><div className="l">{c.l}</div><div className="v">{c.v}</div></div>)}
      </div>
    </div>
  )
}

function CodeTab({ fn, onSaved }: { fn: FunctionInfo; onSaved: () => void }) {
  const [files, setFiles] = useState<{ path: string; content: string }[]>([])
  const [sel, setSel] = useState(0)
  const [lang, setLang] = useState<EditorLang>(langForTemplate(fn.template))
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    setLoading(true); setError(null)
    getFunctionCode(fn.name)
      .then(r => { if (alive) { setFiles(r.files ?? []); setLoading(false) } })
      .catch(e => { if (alive) { setError(String(e)); setLoading(false) } })
    return () => { alive = false }
  }, [fn.name])

  async function save() {
    setBusy(true); setMsg(null)
    try {
      await updateFunction(fn.name, {
        template: fn.template, start: fn.start, port: fn.port, env: fn.env, files,
      })
      setDirty(false); setMsg('Saved ✓ — redeploys on next invoke'); onSaved()
    } catch (e) {
      setMsg(`Save failed: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  if (loading) return <div className="tabpane"><div className="empty">Loading code…</div></div>
  if (error) return <div className="tabpane"><div className="empty" style={{ color: '#dc2626' }}>{error}</div></div>
  if (files.length === 0) return <div className="tabpane"><div className="empty">No code found.</div></div>

  return (
    <div className="tabpane">
      {files.length > 1 && (
        <div className="filetabs">
          {files.map((f, i) => (
            <button key={f.path} className={`filetab${i === sel ? ' on' : ''}`} onClick={() => setSel(i)}>{f.path}</button>
          ))}
        </div>
      )}
      <CodeEditor
        value={files[sel]?.content ?? ''}
        onChange={v => { setFiles(fs => fs.map((f, i) => i === sel ? { ...f, content: v } : f)); setDirty(true) }}
        language={lang}
        onLanguageChange={setLang}
        filename={files[sel]?.path}
        minHeight={320}
        maxHeight={560}
      />
      <div className="savebar">
        <button className="btn primary" disabled={busy || !dirty} onClick={save}>{busy ? 'Saving…' : 'Save code'}</button>
        {dirty && !busy && <span className="hint">Unsaved changes</span>}
        {msg && <span className={`savemsg ${msg.startsWith('Save failed') ? 'err' : 'ok'}`}>{msg}</span>}
      </div>
    </div>
  )
}

function SettingsTab({ fn, onSaved }: { fn: FunctionInfo; onSaved: () => void }) {
  const navigate = useNavigate()
  const [template, setTemplate] = useState(fn.template)
  const [port, setPort] = useState(String(fn.port))
  const [start, setStart] = useState(fn.start)
  const [env, setEnv] = useState<Array<{ k: string; v: string; show: boolean }>>(
    Object.entries(fn.env ?? {}).map(([k, v]) => ({ k, v, show: false }))
  )
  const [msg, setMsg] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function save() {
    setBusy(true); setMsg(null)
    try {
      const envObj: Record<string, string> = {}
      env.forEach(e => { if (e.k.trim()) envObj[e.k.trim()] = e.v })
      await updateFunction(fn.name, { template, start, port: Number(port) || 8000, env: envObj })
      setMsg('Saved ✓'); onSaved()
    } catch (e) {
      setMsg(`Save failed: ${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="tabpane settings">
      <div className="formrow">
        <div className="field" style={{ flex: 1 }}>
          <label>Template</label>
          <select className="sel" value={template} onChange={e => setTemplate(e.target.value)}>
            {['python-3.11', 'node-20'].map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div className="field" style={{ width: 130 }}>
          <label>Port</label>
          <input className="inp mono" value={port} onChange={e => setPort(e.target.value)} />
        </div>
      </div>
      <div className="field">
        <label>Start command</label>
        <input className="inp mono" value={start} onChange={e => setStart(e.target.value)} />
      </div>
      <div className="field">
        <span className="field-label">Environment variables</span>
        {env.length === 0 && <span className="hint">No variables set.</span>}
        {env.map((e, i) => (
          <div className="kvrow" key={i}>
            <input className="inp mono" placeholder="KEY" value={e.k}
              onChange={ev => setEnv(es => es.map((x, j) => j === i ? { ...x, k: ev.target.value } : x))} />
            <input className="inp mono" placeholder="value" type={e.show ? 'text' : 'password'} value={e.v}
              onChange={ev => setEnv(es => es.map((x, j) => j === i ? { ...x, v: ev.target.value } : x))} />
            <button className="iconbtn sm" onClick={() => setEnv(es => es.map((x, j) => j === i ? { ...x, show: !x.show } : x))} aria-label="Toggle visibility">{e.show ? '🙈' : '👁'}</button>
            <button className="iconbtn sm" onClick={() => setEnv(es => es.filter((_, j) => j !== i))} aria-label="Remove">✕</button>
          </div>
        ))}
        <button className="btn ghost" onClick={() => setEnv(es => [...es, { k: '', v: '', show: true }])}>+ Add variable</button>
      </div>
      <div className="savebar">
        <button className="btn primary" disabled={busy} onClick={save}>{busy ? 'Saving…' : 'Save changes'}</button>
        {msg && <span className={`savemsg ${msg.startsWith('Save failed') ? 'err' : 'ok'}`}>{msg}</span>}
      </div>
      <div className="danger">
        <div>
          <b>Delete function</b>
          <p className="hint">Removes the function, its code, and any running sandbox. Cannot be undone.</p>
        </div>
        <button className="btn danger"
          onClick={() => { if (confirm(`Delete function "${fn.name}"? This cannot be undone.`)) deleteFunction(fn.name).then(() => navigate('/functions')).catch(e => alert(`Delete failed: ${errText(e)}`)) }}>
          Delete
        </button>
      </div>
    </div>
  )
}

export default function FunctionDetail() {
  const { name } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const [fn, setFn] = useState<FunctionInfo | null>(null)
  const [metrics, setMetrics] = useState<FunctionMetrics | null>(null)
  const [tab, setTab] = useState<Tab>('overview')

  const refresh = useCallback(() => {
    if (!name) return
    getFunction(name).then(setFn).catch(() => {})
    getFunctionMetrics(name).then(setMetrics).catch(() => {})
  }, [name])

  useEffect(() => { refresh() }, [refresh])
  useEffect(() => {
    const t = setInterval(refresh, 5000)
    return () => clearInterval(t)
  }, [refresh])

  if (!name) return null
  if (!fn) return <div className="wrap"><div className="empty">Loading…</div></div>

  const st = statusOf(fn, metrics)
  const tabs: Array<{ id: Tab; label: string }> = [
    { id: 'overview', label: 'Overview' },
    { id: 'invoke', label: 'Invoke' },
    { id: 'logs', label: 'Logs' },
    { id: 'metrics', label: 'Metrics' },
    { id: 'code', label: 'Code' },
    { id: 'settings', label: 'Settings' },
  ]

  return (
    <div className="detail-wrap">
      <div className="detail-header">
        <button className="back-link" onClick={() => navigate('/functions')}>← Functions</button>
        <div className="detail-title">
          <div><div className="detail-name">{fn.name}</div><div className="detail-id">{fn.template}</div></div>
          <span className={`pill ${st.cls}`}><span className="pd" />{st.label}</span>
        </div>
      </div>
      <div className="tab-bar">
        {tabs.map(t => (
          <button key={t.id} className={`tab-item${tab === t.id ? ' active' : ''}`} onClick={() => setTab(t.id)}>{t.label}</button>
        ))}
      </div>
      <div className="tab-content">
        {tab === 'overview' && <OverviewTab fn={fn} metrics={metrics} onGoInvoke={() => setTab('invoke')} />}
        {tab === 'invoke' && <InvokeTab name={fn.name} onInvoked={refresh} />}
        {tab === 'logs' && <LogsTab name={fn.name} />}
        {tab === 'metrics' && <MetricsTab name={fn.name} />}
        {tab === 'code' && <CodeTab fn={fn} onSaved={refresh} />}
        {tab === 'settings' && <SettingsTab fn={fn} onSaved={refresh} />}
      </div>
    </div>
  )
}

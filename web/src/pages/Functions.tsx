import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { usePoll } from '../hooks/useApi'
import { listFunctions, registerFunction, deleteFunction } from '../api'
import type { FunctionInfo } from '../types'
import CodeEditor from '../components/LazyCodeEditor'
import { type EditorLang, langForTemplate } from '../components/editorLang'
import { errText } from '../lib/errText'

function relTime(unixSecs: number): string {
  if (!unixSecs) return 'never'
  const diff = Math.max(0, Date.now() / 1000 - unixSecs)
  if (diff < 60) return `${Math.floor(diff)}s ago`
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return new Date(unixSecs * 1000).toLocaleDateString()
}

const SAMPLE: Record<EditorLang, string> = {
  python: `import os, json
from http.server import HTTPServer, BaseHTTPRequestHandler

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        payload = json.loads(self.rfile.read(n) or b"{}")
        body = json.dumps({"hello": payload.get("name", "world")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

HTTPServer(("0.0.0.0", int(os.environ["PORT"])), Handler).serve_forever()
`,
  javascript: `const http = require("http");

http.createServer((req, res) => {
  let body = "";
  req.on("data", c => (body += c));
  req.on("end", () => {
    const payload = JSON.parse(body || "{}");
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ hello: payload.name || "world" }));
  });
}).listen(process.env.PORT, "0.0.0.0");
`,
}

const LANG_META: Record<EditorLang, { template: string; start: string; file: string; icon: string }> = {
  python: { template: 'python-3.11', start: 'python3 handler.py', file: 'handler.py', icon: 'py' },
  javascript: { template: 'node-20', start: 'node handler.js', file: 'handler.js', icon: 'js' },
}

function DeployPanel({ onDeployed, onCancel }: { onDeployed: () => void; onCancel: () => void }) {
  const [name, setName] = useState('')
  const [lang, setLang] = useState<EditorLang>('python')
  const [code, setCode] = useState(SAMPLE.python)
  const [touched, setTouched] = useState(false)
  const [env, setEnv] = useState<Array<{ k: string; v: string }>>([])
  const [cpu, setCpu] = useState(1)
  const [mem, setMem] = useState(512)
  const [maxReplicas, setMaxReplicas] = useState(3)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const perHour = (cpu * 0.0000140 + (mem / 1024) * 0.0000045) * 3600

  function switchLang(l: EditorLang) {
    setLang(l)
    if (!touched) setCode(SAMPLE[l])
  }

  async function deploy() {
    setBusy(true); setErr('')
    try {
      const meta = LANG_META[lang]
      const envObj: Record<string, string> = {}
      env.forEach(e => { if (e.k.trim()) envObj[e.k.trim()] = e.v })
      await registerFunction({
        name: name.trim(), template: meta.template, start: meta.start, port: 8000,
        vcpu: cpu, memory_mb: mem, max_replicas: maxReplicas,
        env: envObj, files: [{ path: meta.file, content: code }],
      })
      onDeployed()
    } catch (e) {
      setErr(String(e).replace(/^Error:\s*/, ''))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="panel deploy-panel">
      <div className="ph">
        <h2>New function</h2>
        <button className="iconbtn" onClick={onCancel} aria-label="Close">✕</button>
      </div>
      <div className="deploy-body">
        <div className="field">
          <label htmlFor="fn-name">Name</label>
          <input id="fn-name" className="inp" placeholder="e.g. hello" value={name}
            onChange={e => setName(e.target.value)} autoFocus />
        </div>
        <div className="field">
          <span className="field-label">Handler</span>
          <CodeEditor
            value={code}
            onChange={v => { setCode(v); setTouched(true) }}
            language={lang}
            onLanguageChange={switchLang}
            filename={LANG_META[lang].file}
          />
        </div>
        <div className="field">
          <span className="field-label">Resources <span className="hint">· the limited env this function runs in — bigger costs proportionally more</span></span>
          <div style={{ display: 'flex', gap: 16, alignItems: 'flex-end', flexWrap: 'wrap' }}>
            <label style={{ fontSize: 12, color: 'var(--sub)' }}>CPU<br/>
              <select className="inp" value={cpu} onChange={e => setCpu(Number(e.target.value))} style={{ marginTop: 4 }}>
                {[1, 2, 4, 8].map(v => <option key={v} value={v}>{v} vCPU</option>)}
              </select>
            </label>
            <label style={{ fontSize: 12, color: 'var(--sub)' }}>RAM<br/>
              <select className="inp" value={mem} onChange={e => setMem(Number(e.target.value))} style={{ marginTop: 4 }}>
                {[512, 1024, 2048, 4096, 8192].map(v => <option key={v} value={v}>{v >= 1024 ? `${v / 1024} GB` : `${v} MB`}</option>)}
              </select>
            </label>
            <label style={{ fontSize: 12, color: 'var(--sub)' }}>Autoscale to<br/>
              <select className="inp" value={maxReplicas} onChange={e => setMaxReplicas(Number(e.target.value))} style={{ marginTop: 4 }}>
                {[1, 2, 3, 5, 10].map(v => <option key={v} value={v}>{v} replica{v > 1 ? 's' : ''}</option>)}
              </select>
            </label>
            <div style={{ fontSize: 12, color: 'var(--sub)', paddingBottom: 8 }}>
              ≈ <b style={{ color: 'var(--ink)', fontFamily: 'var(--mono)' }}>${perHour.toFixed(4)}/hr</b> per replica while warm
              <span className="hint"> · scales 1→{maxReplicas} under load (clamped to plan)</span>
            </div>
          </div>
        </div>
        <div className="field">
          <span className="field-label">Environment variables <span className="hint">(optional)</span></span>
          {env.map((e, i) => (
            <div className="kvrow no-eye" key={i}>
              <input className="inp mono" placeholder="KEY" value={e.k}
                onChange={ev => setEnv(es => es.map((x, j) => j === i ? { ...x, k: ev.target.value } : x))} />
              <input className="inp mono" placeholder="value" value={e.v}
                onChange={ev => setEnv(es => es.map((x, j) => j === i ? { ...x, v: ev.target.value } : x))} />
              <button className="iconbtn sm" onClick={() => setEnv(es => es.filter((_, j) => j !== i))} aria-label="Remove">✕</button>
            </div>
          ))}
          <button className="btn ghost" onClick={() => setEnv(es => [...es, { k: '', v: '' }])}>+ Add variable</button>
        </div>
        {err && <div className="savemsg err">{err}</div>}
        <div className="savebar">
          <button className="btn primary" disabled={busy || !name.trim()} onClick={deploy}>
            {busy ? 'Deploying…' : 'Deploy function'}
          </button>
          <button className="btn" onClick={onCancel}>Cancel</button>
        </div>
      </div>
    </div>
  )
}

export default function Functions() {
  const navigate = useNavigate()
  const { data, refetch } = usePoll<{ functions: FunctionInfo[] }>(listFunctions, 3000)
  const [open, setOpen] = useState(false)
  const fns = data?.functions ?? []
  const running = fns.filter(f => f.sandbox_id).length

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Functions</h1>
          <p>
            {fns.length} function{fns.length !== 1 ? 's' : ''} · <b>{running} warm</b> · serverless HTTP handlers
          </p>
        </div>
        {!open && <button className="btn primary" onClick={() => setOpen(true)}>＋ New function</button>}
      </div>

      {open && <DeployPanel onDeployed={() => { setOpen(false); refetch() }} onCancel={() => setOpen(false)} />}

      <div className="panel" style={{ marginTop: open ? 20 : 0 }}>
        <div className="ph">
          <h2>Registered</h2>
          <span className="meta">auto · 3s</span>
        </div>

        {fns.length === 0 ? (
          <div className="empty">
            No functions yet
            <p>Deploy your first serverless function with the button above.</p>
          </div>
        ) : (
          <>
            <div className="rowh fn">
              <div>Function</div>
              <div>Status</div>
              <div>Invokes</div>
              <div>Last invoked</div>
              <div>Port</div>
              <div />
            </div>
            {fns.map(fn => {
              const icon = LANG_META[langForTemplate(fn.template)].icon
              const warm = !!fn.sandbox_id
              return (
                <div
                  className="row fn row-link"
                  key={fn.name}
                  role="link"
                  tabIndex={0}
                  onClick={() => navigate(`/fn/${fn.name}`)}
                  onKeyDown={e => {
                    if (e.target !== e.currentTarget) return
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault()
                      navigate(`/fn/${fn.name}`)
                    }
                  }}
                >
                  <div className="sb">
                    <div className="ic">{icon}</div>
                    <div className="nm">{fn.name}<small>{fn.template}</small></div>
                  </div>
                  <div>
                    <span className={`pill ${warm ? 'run' : 'pau'}`}><span className="pd" />{warm ? 'Warm' : 'Cold'}</span>
                  </div>
                  <div>{fn.invokes}</div>
                  <div style={{ color: 'var(--sub)' }}>{relTime(fn.last_invoked_at)}</div>
                  <div className="port" style={{ fontFamily: 'var(--mono)' }}>:{fn.port}</div>
                  <div className="rowact" onClick={e => e.stopPropagation()}>
                    <button className="iconbtn" title="Delete function" aria-label="Delete function"
                      onClick={() => { if (confirm(`Delete function "${fn.name}"?`)) deleteFunction(fn.name).then(refetch).catch(e => alert(`Delete failed: ${errText(e)}`)) }}>
                      ✕
                    </button>
                  </div>
                </div>
              )
            })}
          </>
        )}
      </div>
    </div>
  )
}

import { useState, useEffect, useRef, useCallback } from 'react'
import { useParams, useNavigate, useLocation } from 'react-router-dom'
import { getSandbox, getSandboxLogs, pauseSandbox, resumeSandbox, forkSandbox, refreshSandbox, destroySandbox, listTemplates, isPreviewCrossOrigin, getSandboxMetrics, getSandboxPorts, setSandboxSlug, slugUrl, setSandboxVisibility, absolutePreviewUrl, ApiError, IS_LOCAL_RUNTIME } from '../api'
import UnsafeEmbedNotice from '../components/UnsafeEmbedNotice'
import { useConfirm } from '../components/ConfirmModal'
import { useToast } from '../components/Toast'
import { errText } from '../lib/errText'
import { cpuReading, memReading, diskReading } from '../lib/liveMetrics'
import FileBrowser from '../components/FileBrowser'
import type { SandboxDetail as SandboxDetailType, Template, SandboxMetrics, PortStatus } from '../types'
import { statusPill } from './Sandboxes'
import { TemplateIcon } from '../components/templateIcons'
import { usePreviewLink } from '../lib/previewToken'
import { previewPortOf } from '../lib/previewPort'
import SandboxTerminal from '../components/SandboxTerminal'

function formatDate(ts: string): string {
  try {
    return new Date(ts).toLocaleString('en-GB', { hour12: false })
  } catch {
    return ts
  }
}

type Tab = 'shell' | 'terminal' | 'files' | 'preview' | 'logs' | 'info'

function defaultTabFor(outcome?: string): Tab {
  switch (outcome) {
    case 'screen':
    case 'report':
    case 'verdict':
    case 'preview':
    case 'endpoint':
      return 'preview'
    default:
      return 'shell'
  }
}

/** previewLabel names the Preview tab after the manifest's outcome. */
function previewLabel(outcome?: string): string {
  switch (outcome) {
    case 'screen': return 'Screen'
    case 'report': return 'Report'
    case 'verdict': return 'Verdict'
    case 'endpoint': return 'Endpoint'
    case 'connection': return 'Connection'
    default: return 'Preview'
  }
}

type QuickCmd = { label: string; cmd: string }
function quickStart(template: string): { cmds: QuickCmd[] } {
  const t = (template || '').toLowerCase()
  const has = (...w: string[]) => w.some(x => t.includes(x))

  if (has('code-interpreter', 'python', 'jupyter', 'fastapi', 'flask', 'django', 'openclaw', 'py')) {
    return {
      cmds: [
        { label: 'Python version', cmd: 'python3 --version' },
        {
          label: 'pandas demo',
          cmd: `python3 -c "import pandas as pd; print(pd.DataFrame({'x':[1,2,3],'y':[4,5,6]}).describe())"`,
        },
        { label: 'Installed packages', cmd: 'pip list' },
      ],
    }
  }
  if (has('node', 'typescript', 'next', 'js', 'deno', 'bun')) {
    return {
      cmds: [
        { label: 'Node version', cmd: 'node --version' },
        { label: 'Run JS', cmd: `node -e "console.log('hello from', process.version)"` },
        { label: 'Global packages', cmd: 'npm ls -g --depth=0' },
      ],
    }
  }
  if (has('golang') || t === 'go' || t.startsWith('go')) {
    return {
      cmds: [
        { label: 'Go version', cmd: 'go version' },
        { label: 'Go env', cmd: 'go env GOPATH GOROOT' },
      ],
    }
  }
  if (has('rust', 'cargo')) {
    return {
      cmds: [
        { label: 'rustc version', cmd: 'rustc --version' },
        { label: 'cargo version', cmd: 'cargo --version' },
      ],
    }
  }
  return {
    cmds: [
      { label: 'System info', cmd: 'uname -a' },
      { label: 'List files', cmd: 'ls -la' },
      { label: 'Working dir', cmd: 'pwd' },
    ],
  }
}

const ENVD_TERMINAL_PORT = 7681

function QuickCommands({ template, onRun }: { template: string; onRun?: (cmd: string) => void }) {
  const { cmds } = quickStart(template)
  const [copied, setCopied] = useState<string | null>(null)

  function activate(cmd: string) {
    if (onRun) {
      onRun(cmd)
      return
    }
    navigator.clipboard?.writeText(cmd).then(
      () => {
        setCopied(cmd)
        window.setTimeout(() => setCopied(c => (c === cmd ? null : c)), 1500)
      },
      () => {},
    )
  }

  return (
    <div className="term-quick">
      {cmds.map(c => (
        <button
          key={c.cmd}
          type="button"
          className="term-quick-btn"
          title={c.cmd}
          onClick={() => activate(c.cmd)}
        >
          {copied === c.cmd ? '✓ copied' : c.label}
        </button>
      ))}
    </div>
  )
}

export function ShellTab({
  id,
  status,
  template,
}: {
  id: string
  status: string
  template: string
}) {
  const isRunning = status === 'running'
  const url = usePreviewLink(id, ENVD_TERMINAL_PORT)
  const termRef = useRef<((cmd: string) => void) | null>(null)

  if (!isRunning) {
    return (
      <div className="term-wrap">
        <NotRunningNotice status={status} what="open the terminal" />
      </div>
    )
  }
  return (
    <div className="term-wrap-outer">
      <div className="preview-toolbar">
        <span style={{ fontSize: 13, color: 'var(--sub)', fontWeight: 500 }}>
          Live shell (PTY) — arrow keys, ctrl-c and TUIs all work
        </span>
        {url && (
          <a href={url} target="_blank" rel="noopener noreferrer" className="btn">
            Open in new tab ↗
          </a>
        )}
      </div>
      <QuickCommands template={template} onRun={cmd => termRef.current?.(cmd)} />
      {url ? (
        <SandboxTerminal wsPath={wsPathFor(url)} onReady={fn => { termRef.current = fn }} />
      ) : (
        <div className="term-wrap"><div className="empty">Connecting…</div></div>
      )}
    </div>
  )
}

export function wsPathFor(previewUrl: string): string {
  const [path, query] = previewUrl.split('?')
  return `${path.replace(/\/$/, '')}/ws${query ? `?${query}` : ''}`
}

export function PreviewTab({
  id,
  status,
  defaultPort,
  outcome,
}: {
  id: string
  status: string
  defaultPort?: number
  outcome?: string
}) {
  const seed = String(defaultPort || 8000)
  const [port, setPort] = useState(seed)
  const [activePort, setActivePort] = useState(seed)
  const isRunning = status === 'running'

  const [detected, setDetected] = useState<PortStatus[] | null>(null)
  useEffect(() => {
    if (!isRunning) { setDetected(null); return }
    let alive = true
    getSandboxPorts(id)
      .then(r => { if (alive) setDetected(r.ports ?? []) })
      .catch(() => { if (alive) setDetected(null) })
    return () => { alive = false }
  }, [id, isRunning])

  const listening = (detected ?? []).filter(p => p.listening)

  function choosePort(p: number) {
    const s = String(p)
    setPort(s)
    setActivePort(s)
  }

  const iframeUrl = usePreviewLink(id, activePort)
  // The preview root is a directory listing whose root-relative links 404 under
  // path-form previews, so the report gets a direct link.
  const [previewPath, previewQuery] = iframeUrl.split('?')
  const reportUrl = `${previewPath}report/${previewQuery ? `?${previewQuery}` : ''}`

  function previewBody() {
    if (!isRunning) return <NotRunningNotice status={status} what="preview your application" />
    if (!isPreviewCrossOrigin()) return <UnsafeEmbedNotice url={iframeUrl} what="preview" />
    return (
      <iframe
        className="preview-iframe"
        src={iframeUrl}
        title={`Preview — port ${activePort}`}
        sandbox="allow-scripts allow-same-origin allow-forms allow-popups"
      />
    )
  }

  return (
    <div className="preview-wrap">
      <div className="preview-toolbar">
        <span style={{ fontSize: 13, color: 'var(--sub)', fontWeight: 500 }}>Port</span>
        <input
          className="preview-port-input"
          type="number"
          value={port}
          onChange={e => setPort(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') setActivePort(port) }}
          min={1}
          max={65535}
          placeholder="8000"
          aria-label="Preview port"
        />
        <button
          className="btn"
          onClick={() => setActivePort(port)}
          disabled={!isRunning}
        >
          Load
        </button>
        {isRunning && (
          <a
            href={iframeUrl}
            target="_blank"
            rel="noopener noreferrer"
            className="btn"
          >
            Open in new tab ↗
          </a>
        )}
        {isRunning && outcome === 'report' && iframeUrl && (
          <a href={reportUrl} target="_blank" rel="noopener noreferrer" className="btn primary">
            View report ↗
          </a>
        )}
      </div>

      {listening.length > 0 && (
        <div className="preview-detected-ports">
          <span className="preview-detected-label">
            Detected ports — common ports we probed and got an answer from; an app on an unusual port may not appear
          </span>
          <div className="preview-port-chips">
            {listening.map(p => (
              <button
                key={p.port}
                type="button"
                className={`port-chip${String(p.port) === activePort ? ' active' : ''}`}
                onClick={() => choosePort(p.port)}
                title={p.port === defaultPort ? 'Declared preview port' : undefined}
              >
                {p.port}{p.port === defaultPort ? ' ★' : ''}
              </button>
            ))}
          </div>
        </div>
      )}

      <div className="preview-frame-wrap">
        {previewBody()}
      </div>
    </div>
  )
}

function NotRunningNotice({ status, what }: { status: string; what: string }) {
  const crashed = status === 'crashed'
  const exited = status === 'exited'

  function reason() {
    if (crashed) {
      return (
        <>
          <span>Sandbox crashed — nothing is serving</span>
          <span style={{ fontSize: 12, color: 'var(--faint)' }}>
            The Logs tab has the exit. Fix the start command or clone and relaunch — resuming replays the same crash.
          </span>
        </>
      )
    }
    if (exited) {
      return (
        <>
          <span>Sandbox has stopped</span>
          <span style={{ fontSize: 12, color: 'var(--faint)' }}>Its process exited. Clone it to run again.</span>
        </>
      )
    }
    return <span>Sandbox is paused — resume it to {what}</span>
  }

  return (
    <div className="preview-paused">
      <svg viewBox="0 0 24 24">
        {crashed || exited ? (
          <path d="M12 4l8 14H4z M12 10v4 M12 17v.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
        ) : (
          <>
            <rect x="6" y="4" width="4" height="16" rx="1" />
            <rect x="14" y="4" width="4" height="16" rx="1" />
          </>
        )}
      </svg>
      {reason()}
    </div>
  )
}

// Colour and cursor escapes; the feed is plain text, so they would show as junk.
// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g

export function LogsTab({ id }: { id: string }) {
  const [lines, setLines] = useState<string[]>([])
  const [note, setNote] = useState('')
  const [truncated, setTruncated] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)
  const [autoScroll, setAutoScroll] = useState(true)
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let alive = true
    const load = () =>
      getSandboxLogs(id)
        .then(r => {
          if (!alive) return
          setLines((r.lines ?? []).map(l => l.replace(ANSI_RE, '')))
          setNote(r.note ?? '')
          setTruncated(r.truncated ?? false)
        })
        .catch(() => {})
    load()
    const t = setInterval(load, 3000)
    return () => { alive = false; clearInterval(t) }
  }, [id])

  useEffect(() => {
    if (autoScroll && bottomRef.current) bottomRef.current.scrollIntoView()
  }, [lines, autoScroll])

  // Scrolling up to read stops following; the checkbox turns it back on.
  function handleScroll() {
    const el = containerRef.current
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight >= 60) setAutoScroll(false)
  }

  return (
    <div style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 10, minHeight: 0 }}>
      <div className="logbar">
        <span className="hint">{lines.length} lines</span>
        <label className="chk">
          <input type="checkbox" checked={autoScroll} onChange={e => setAutoScroll(e.target.checked)} /> auto-scroll
        </label>
      </div>
      <div
        ref={containerRef}
        onScroll={handleScroll}
        className="log-feed"
        style={{
          flex: 1, overflowY: 'auto', fontFamily: 'var(--mono)', fontSize: 12.5,
          background: 'var(--bg)', border: '1px solid var(--line)', borderRadius: 10, padding: 14,
        }}
      >
        {truncated && (
          <div style={{ color: 'var(--faint)', marginBottom: 6 }}>… older output truncated</div>
        )}
        {lines.length === 0 ? (
          <div className="empty" style={{ padding: '60px 20px' }}>
            {note || 'No app output yet.'}
            {!note && <p>stdout and stderr of the start command appear here.</p>}
          </div>
        ) : (
          lines.map((l, i) => (
            <div key={i} style={{ whiteSpace: 'pre-wrap', lineHeight: 1.55 }}>{l}</div>
          ))
        )}
        <div ref={bottomRef} />
      </div>
    </div>
  )
}

const fmtMB = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB` : `${mb} MB`)

// SlugRow shows the slug and its host, editable in place; the server owns the
// grammar and uniqueness, so an error is shown verbatim.
function SlugRow({ sandbox, onChanged }: { sandbox: SandboxDetailType; onChanged: (slug: string) => void }) {
  const [value, setValue] = useState(sandbox.slug ?? '')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const toast = useToast()
  const host = slugUrl(sandbox.slug)
  async function save() {
    setBusy(true)
    setErr('')
    try {
      const updated = await setSandboxSlug(sandbox.id, value.trim())
      onChanged(updated.slug ?? '')
      toast.success(updated.slug ? `Slug set to ${updated.slug}` : 'Slug cleared')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="info-row">
      <div className="info-key">Slug</div>
      <div className="info-val">
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            className="sb-name-input"
            style={{ flex: 1, minWidth: 0 }}
            value={value}
            disabled={busy}
            maxLength={63}
            placeholder="myapp"
            aria-label="Sandbox slug"
            onChange={e => setValue(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void save() }}
          />
          <button className="btn" disabled={busy || value.trim() === (sandbox.slug ?? '')} onClick={() => void save()}>
            Save
          </button>
        </div>
        {host && (
          <div>
            <a href={host} target="_blank" rel="noopener noreferrer">{host}</a>
          </div>
        )}
        {err && <div style={{ color: 'var(--danger, #c33)' }}>{err}</div>}
      </div>
    </div>
  )
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="copy-btn"
      aria-label={`Copy ${label}`}
      onClick={() =>
        navigator.clipboard?.writeText(text).then(
          () => {
            setCopied(true)
            window.setTimeout(() => setCopied(false), 1400)
          },
          () => {},
        )
      }
    >
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

function storageLabel(sb: SandboxDetailType): string {
  if (sb.storage_used_mb !== undefined) {
    return `${fmtMB(sb.storage_used_mb)} of ${sb.storage_mb ? fmtMB(sb.storage_mb) : '—'}`
  }
  return sb.storage_mb ? `${fmtMB(sb.storage_mb)} (usage not reported)` : '—'
}

export function InfoTab({
  sandbox,
  previewPort,
  onSlug,
}: {
  sandbox: SandboxDetailType
  previewPort: number
  onSlug?: (slug: string) => void
}) {
  const previewUrl = absolutePreviewUrl(sandbox.id, previewPort)
  const rows: Array<{ key: string; value: string; copy?: string }> = [
    { key: 'Name', value: sandbox.name || '—' },
    { key: 'ID', value: sandbox.id, copy: sandbox.id },
    { key: 'Template', value: sandbox.template },
    { key: 'Status', value: sandbox.status },
    { key: 'API access', value: sandbox.visibility === 'private' ? 'Only the key that created it' : 'Everyone in the org' },
    { key: 'Preview URL', value: previewUrl, copy: previewUrl },
    {
      key: 'Ports',
      value: `${previewPort} preview · ${ENVD_TERMINAL_PORT} terminal${sandbox.desktop_url ? ' · desktop' : ''}`,
    },
    { key: 'Boot type', value: sandbox.boot_type || '—' },
    { key: 'Boot time', value: sandbox.boot_ms > 0 ? `${sandbox.boot_ms} ms` : '—' },
    { key: 'Created', value: formatDate(sandbox.created_at) },
    { key: 'Expires', value: formatDate(sandbox.expires_at) },
    { key: 'Storage', value: storageLabel(sandbox) },
  ]

  return (
    <div style={{ flex: 1 }}>
      <div style={{ marginBottom: 20 }}>
        {(() => { const p = statusPill(sandbox.status || 'running'); return (
          <span className="pill" style={{ background: p.bg, color: p.fg, fontSize: 14 }}>
            <span className="pd" style={{ background: p.fg }} />
            {p.label}
          </span>
        ) })()}
      </div>

      <div className="info-grid">
        {rows.map(r => (
          <div className="info-row" key={r.key}>
            <div className="info-key">{r.key}</div>
            <div className="info-val">
              {r.value}
              {r.copy && <> <CopyButton text={r.copy} label={r.key} /></>}
            </div>
          </div>
        ))}
        <SlugRow sandbox={sandbox} onChanged={slug => onSlug?.(slug)} />
      </div>
    </div>
  )
}

const MEM_WARN_RATIO = 0.85

function formatUptime(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)}s`
  const mins = Math.floor(seconds / 60)
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  return `${hrs}h ${mins % 60}m`
}

export function MetricsStrip({ id, status }: { id: string; status: string }) {
  const [m, setM] = useState<SandboxMetrics | null>(null)
  const isRunning = status === 'running'

  useEffect(() => {
    if (!isRunning) return
    let alive = true
    const load = () => {
      getSandboxMetrics(id)
        .then(r => { if (alive) setM(r) })
        .catch(() => {})
    }
    load()
    const t = setInterval(load, 5000)
    return () => { alive = false; clearInterval(t) }
  }, [id, isRunning])

  if (!isRunning) return null

  const cpu = cpuReading(m)
  const mem = memReading(m)
  const disk = diskReading(m)
  const uptime = m?.uptime_seconds

  const memRatio = m?.mem_used_mib != null && m?.mem_mib ? m.mem_used_mib / m.mem_mib : null
  const memWarn = memRatio != null && memRatio >= MEM_WARN_RATIO

  return (
    <div className="metrics-strip">
      {cpu && (
        <div
          className="sbmetric"
          data-testid="metric-cpu"
          title={cpu.kind === 'utilisation'
            ? 'CPU utilization (100% = one core)'
            : 'vCPU allocated to this sandbox. Utilization is not reported for this runtime.'}
        >
          <span className="sbmetric-label">{cpu.kind === 'utilisation' ? 'CPU' : 'CPU size'}</span>
          <span className="sbmetric-value">{cpu.text}</span>
        </div>
      )}
      {mem && (
        <div
          className={`sbmetric${memWarn ? ' sbmetric-warn' : ''}`}
          data-testid="metric-mem"
          title={mem.kind === 'utilisation'
            ? "Resident memory against the sandbox's memory cap"
            : 'Memory allocated to this sandbox. Usage is not reported for this runtime.'}
        >
          <span className="sbmetric-label">{mem.kind === 'utilisation' ? 'Memory' : 'Memory size'}</span>
          <span className="sbmetric-value">{mem.text}</span>
          {memWarn && (
            <span className="sbmetric-warning" role="status" data-testid="metric-mem-warning">
              ⚠ near memory cap
            </span>
          )}
        </div>
      )}
      {disk && (
        <div className="sbmetric" data-testid="metric-disk" title="Writable-layer disk usage">
          <span className="sbmetric-label">Disk</span>
          <span className="sbmetric-value">{disk.text}</span>
        </div>
      )}
      <div className="sbmetric" data-testid="metric-uptime">
        <span className="sbmetric-label">Uptime</span>
        <span className="sbmetric-value">{uptime != null ? formatUptime(uptime) : '—'}</span>
      </div>
    </div>
  )
}

export default function SandboxDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const location = useLocation()

  const [tab, setTabRaw] = useState<Tab>('shell')
  // Mounted on first visit and then only hidden, so a tab switch keeps the shell and its history.
  const [shellMounted, setShellMounted] = useState(false)
  if (tab === 'shell' && !shellMounted) setShellMounted(true)
  const userPickedTab = useRef(false)
  const setTab = (t: Tab) => {
    userPickedTab.current = true
    setTabRaw(t)
  }
  const [sandbox, setSandbox] = useState<SandboxDetailType | null>(
    (location.state as { sandbox?: SandboxDetailType })?.sandbox ?? null
  )
  const [tplMeta, setTplMeta] = useState<Template | null>(null)
  useEffect(() => {
    let alive = true
    listTemplates()
      .then(ts => {
        if (!alive) return
        const templateId = sandbox?.template
        if (templateId) setTplMeta(ts.find(t => t.id === templateId) ?? null)
      })
      .catch(() => {})
    return () => {
      alive = false
    }

  }, [sandbox?.template])
  useEffect(() => {
    if (userPickedTab.current || !tplMeta) return
    setTabRaw(defaultTabFor(tplMeta.outcome))
  }, [tplMeta])
  const [loading, setLoading] = useState(!sandbox)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  // Cleared on a 404 too, so a destroyed sandbox is not polled until unmount.
  const pollRef = useRef<number | null>(null)
  const loadSandbox = useCallback(async () => {
    if (!id) return
    try {
      const data = await getSandbox(id)
      // The detail GET omits boot type/time; keep what the create response said.
      setSandbox(prev => prev?.id === data.id
        ? { ...data, boot_type: data.boot_type || prev.boot_type, boot_ms: data.boot_ms || prev.boot_ms }
        : data)
      setError(null)
    } catch (e) {
      setError((e as Error).message)
      if (e instanceof ApiError && e.status === 404 && pollRef.current !== null) {
        window.clearInterval(pollRef.current)
        pollRef.current = null
      }
    }
    setLoading(false)
  }, [id])

  useEffect(() => {
    loadSandbox()
    pollRef.current = window.setInterval(loadSandbox, 5000)
    return () => {
      if (pollRef.current !== null) window.clearInterval(pollRef.current)
    }
  }, [loadSandbox])

  const previewPort = sandbox ? previewPortOf(sandbox, tplMeta) : 8000
  const openUrl = usePreviewLink(sandbox?.id, previewPort)

  const confirmDialog = useConfirm()
  const toast = useToast()
  const doAction = useCallback(async (fn: () => Promise<unknown>, confirmMsg?: string, successMsg?: string) => {
    if (confirmMsg && !(await confirmDialog({
      title: 'Destroy sandbox?',
      body: confirmMsg,
      confirmLabel: 'Destroy',
      danger: true,
    }))) return
    setBusy(true)
    try {
      await fn()
      await loadSandbox()
      if (successMsg) toast.success(successMsg)
    } catch (e) {
      toast.error(errText(e))
    }
    setBusy(false)
  }, [loadSandbox, confirmDialog, toast])

  const isPrivate = sandbox?.visibility === 'private'

  // Sharing wipes the agent logins in the sandbox and cannot be undone, so it
  // asks first and says exactly what goes; unsharing only narrows who can see
  // it, and restores nothing that a previous share removed.
  const shareToggle = useCallback(async () => {
    if (!id) return
    if (isPrivate && !(await confirmDialog({
      title: 'Share with the org?',
      body: 'Everyone with a key in your org will be able to open, exec in and destroy this sandbox. '
        + 'Its Claude, Codex and opencode logins are deleted first, and are not restored if you make it '
        + 'private again — whoever was signed in there has to sign in again.',
      confirmLabel: 'Share',
      danger: true,
    }))) return
    await doAction(async () => {
      const res = await setSandboxVisibility(id, isPrivate ? 'org' : 'private')
      if (res.wipe_skipped) toast.error(`Shared, but the agent logins were NOT removed: ${res.wipe_skipped}`)
      else toast.success(isPrivate ? 'Shared with the org' : 'Made private')
    })
  }, [id, isPrivate, doAction, confirmDialog, toast])

  if (!id) return null

  if (loading && !sandbox) {
    return (
      <div className="detail-wrap">
        <div className="detail-loading">Loading sandbox…</div>
      </div>
    )
  }

  if (error && !sandbox) {
    return (
      <div className="detail-wrap">
        <button className="back-link" onClick={() => navigate('/')}>← Sandboxes</button>
        <div className="detail-loading" style={{ color: '#dc2626' }}>{error}</div>
      </div>
    )
  }

  const sb = sandbox!
  const isRunning = sb.status === 'running'

  const tabs: Array<{ id: Tab; label: string }> = [
    { id: 'shell', label: 'Terminal' },
    { id: 'files', label: 'Files' },
    { id: 'preview', label: previewLabel(tplMeta?.outcome) },
    { id: 'logs', label: 'Logs' },
    { id: 'info', label: 'Info' },
  ]

  return (
    <div className="detail-wrap">
      <div className="detail-header">
        <button className="back-link" onClick={() => navigate('/')}>
          ← Sandboxes
        </button>

        <div className="detail-title">
          <div className="ic" style={{ width: 36, height: 36, borderRadius: 10 }}>
            <TemplateIcon template={sb.template} />
          </div>
          <div>
            <div className="detail-name">{sb.name || sb.template}</div>
            <div className="detail-id">{sb.id}</div>
          </div>
          {(() => { const p = statusPill(sb.status || 'running'); return (
            <span className="pill" style={{ background: p.bg, color: p.fg }}>
              <span className="pd" style={{ background: p.fg }} />
              {p.label}
            </span>
          ) })()}
          <span className="chip">{sb.template}</span>
        </div>

        <div className="detail-actions">
          {isRunning && (
            <a
              href={openUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="btn"
            >
              Open preview ↗
            </a>
          )}
          {!IS_LOCAL_RUNTIME && (isRunning ? (
            <button
              className="btn"
              disabled={busy}
              onClick={() => doAction(() => pauseSandbox(id), undefined, 'Paused')}
            >
              Pause
            </button>
          ) : (
            <button
              className="btn"
              disabled={busy}
              onClick={() => doAction(() => resumeSandbox(id), undefined, 'Resumed')}
            >
              Resume
            </button>
          ))}
          {!IS_LOCAL_RUNTIME && <>
          <button
            className="btn"
            disabled={busy}
            title="Copy this sandbox, memory and running processes included"
            onClick={() => doAction(
              () => forkSandbox(id).then((f) => { navigate(`/sandbox/${f.id}`); return f }),
              undefined,
              'Forked',
            )}
          >
            Fork
          </button>
          <label
            className="detail-access"
            title="Who can open, exec in and destroy this sandbox through the API. It does not change who can open the preview URL."
          >
            API access
            <select
              className="sel"
              disabled={busy}
              value={isPrivate ? 'private' : 'org'}
              onChange={e => { if ((e.target.value === 'private') !== isPrivate) void shareToggle() }}
            >
              <option value="org">Whole org</option>
              <option value="private">Only my key</option>
            </select>
          </label>
          </>}
          <button
            className="btn"
            disabled={busy}
            onClick={() => doAction(() => refreshSandbox(id), undefined, 'TTL refreshed')}
            title="Extend TTL"
          >
            Refresh TTL
          </button>
          <button
            className="btn danger"
            disabled={busy}
            onClick={() =>
              doAction(
                () => destroySandbox(id).then(() => navigate('/')),
                `Destroy sandbox ${sb.id}? This cannot be undone.`
              )
            }
          >
            Destroy
          </button>
        </div>
        {error && sandbox && (
          <p className="lm-error" style={{ marginTop: 10 }}>{error}</p>
        )}
      </div>

      <MetricsStrip id={id} status={sb.status} />

      <div className="tab-bar">
        {tabs.map(t => (
          <button
            key={t.id}
            className={`tab-item${tab === t.id ? ' active' : ''}`}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div className="tab-content">
        {shellMounted && (
          <div style={{ display: tab === 'shell' ? 'contents' : 'none' }}>
            <ShellTab id={id} status={sb.status} template={sb.template} />
          </div>
        )}
        {tab === 'files' && <FileBrowser id={id} />}
        {tab === 'preview' && (
          <PreviewTab
            id={id}
            status={sb.status}
            defaultPort={previewPort}
            outcome={tplMeta?.outcome}
          />
        )}
        {tab === 'logs' && <LogsTab id={id} />}
        {tab === 'info' && <InfoTab sandbox={sb} previewPort={previewPort} onSlug={slug => setSandbox(prev => (prev ? { ...prev, slug: slug || undefined } : prev))} />}
      </div>
    </div>
  )
}

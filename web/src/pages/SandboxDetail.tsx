import { useState, useEffect, useRef, useCallback } from 'react'
import { useParams, useNavigate, useLocation } from 'react-router-dom'
import { getChannels, getSandbox, getSandboxLogs, pauseSandbox, resumeSandbox, forkSandbox, refreshSandbox, destroySandbox, listTemplates, isPreviewCrossOrigin, getSandboxMetrics, getSandboxPorts, createChatLink, setSandboxSlug, slugUrl, setSandboxVisibility, IS_LOCAL_RUNTIME } from '../api'
import UnsafeEmbedNotice from '../components/UnsafeEmbedNotice'
import { useConfirm } from '../components/ConfirmModal'
import { useToast } from '../components/Toast'
import { errText } from '../lib/errText'
import { cpuReading, memReading, diskReading } from '../lib/liveMetrics'
import FileBrowser from '../components/FileBrowser'
import { ModelTab, KeysTab, IntegrationsTab, ChannelsTab } from '../components/WorkstationTabs'
import { ConnectCard } from '../components/agent/ConnectCard'
import { TimelineTab } from '../components/agent/TimelineTab'
import { agentTabsVisible, previewLabel } from './agentTabGate'
import { IsolationCard } from '../components/agent/IsolationCard'
import { AgentPanel } from '../components/agent/AgentPanel'
import type { ChatLinkResp } from '../api'
import type { SandboxDetail as SandboxDetailType, Template, SandboxMetrics, PortStatus } from '../types'
import { statusPill } from './Sandboxes'
import { TemplateIcon } from '../components/templateIcons'
import { usePreviewLink } from '../lib/previewToken'
import SandboxTerminal from '../components/SandboxTerminal'

function formatDate(ts: string): string {
  try {
    return new Date(ts).toLocaleString('en-GB', { hour12: false })
  } catch {
    return ts
  }
}

type Tab = 'agent' | 'timeline' | 'connect' | 'shell' | 'terminal' | 'files' | 'preview' | 'logs' | 'info' | 'model' | 'keys' | 'integrations' | 'channels'

function defaultTabFor(outcome?: string, kind?: string): Tab {
  if (kind === 'agent') return 'agent'
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

type QuickCmd = { label: string; cmd: string }
function quickStart(template: string): { intro: string; cmds: QuickCmd[] } {
  const t = (template || '').toLowerCase()
  const has = (...w: string[]) => w.some(x => t.includes(x))

  if (has('code-interpreter', 'python', 'jupyter', 'fastapi', 'flask', 'django', 'openclaw', 'py')) {
    return {
      intro: 'Python 3.11 with numpy, pandas & matplotlib is ready. Try one:',
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
      intro: 'Node 20 + TypeScript is ready. Try one:',
      cmds: [
        { label: 'Node version', cmd: 'node --version' },
        { label: 'Run JS', cmd: `node -e "console.log('hello from', process.version)"` },
        { label: 'Global packages', cmd: 'npm ls -g --depth=0' },
      ],
    }
  }
  if (has('golang') || t === 'go' || t.startsWith('go')) {
    return {
      intro: 'Go 1.22 toolchain is ready. Try one:',
      cmds: [
        { label: 'Go version', cmd: 'go version' },
        { label: 'Go env', cmd: 'go env GOPATH GOROOT' },
      ],
    }
  }
  if (has('rust', 'cargo')) {
    return {
      intro: 'Rust toolchain is ready. Try one:',
      cmds: [
        { label: 'rustc version', cmd: 'rustc --version' },
        { label: 'cargo version', cmd: 'cargo --version' },
      ],
    }
  }
  return {
    intro: 'A minimal Debian shell. Try one:',
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
}: {
  id: string
  status: string
  defaultPort?: number
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

function LogsTab({ id }: { id: string }) {
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
          setLines(r.lines ?? [])
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

  function handleScroll() {
    const el = containerRef.current
    if (!el) return
    setAutoScroll(el.scrollHeight - el.scrollTop - el.clientHeight < 60)
  }

  return (
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
  )
}

const fmtMB = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB` : `${mb} MB`)

// SlugRow shows the slug and its host, editable in place; the server owns the
// grammar and uniqueness, so an error is shown verbatim.
function SlugRow({ sandbox, onChanged }: { sandbox: SandboxDetailType; onChanged: (slug: string) => void }) {
  const [value, setValue] = useState(sandbox.slug ?? '')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const host = slugUrl(sandbox.slug)
  async function save() {
    setBusy(true)
    setErr('')
    try {
      const updated = await setSandboxSlug(sandbox.id, value.trim())
      onChanged(updated.slug ?? '')
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
        <input
          className="sb-name-input"
          value={value}
          disabled={busy}
          maxLength={63}
          placeholder="myapp"
          aria-label="Sandbox slug"
          onChange={e => setValue(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') void save() }}
        />
        <button className="btn" disabled={busy || value.trim() === (sandbox.slug ?? '')} onClick={() => void save()} style={{ marginLeft: 8 }}>
          Save
        </button>
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

function InfoTab({ sandbox, onSlug }: { sandbox: SandboxDetailType; onSlug?: (slug: string) => void }) {
  const rows: Array<{ key: string; value: string }> = [
    { key: 'ID', value: sandbox.id },
    { key: 'Template', value: sandbox.template },
    { key: 'Status', value: sandbox.status },
    { key: 'Boot type', value: sandbox.boot_type || '—' },
    { key: 'Boot time', value: sandbox.boot_ms > 0 ? `${sandbox.boot_ms} ms` : '—' },
    { key: 'Created', value: formatDate(sandbox.created_at) },
    { key: 'Expires', value: formatDate(sandbox.expires_at) },
    {
      key: 'Storage',
      value:
        sandbox.storage_used_mb !== undefined
          ? `${fmtMB(sandbox.storage_used_mb)} of ${sandbox.storage_mb ? fmtMB(sandbox.storage_mb) : '—'}`
          : '—',
    },
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
            <div className="info-val">{r.value}</div>
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
    setTabRaw(defaultTabFor(tplMeta.outcome, tplMeta.kind))
  }, [tplMeta])
  const [loading, setLoading] = useState(!sandbox)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const loadSandbox = useCallback(async () => {
    if (!id) return
    try {
      const data = await getSandbox(id)
      setSandbox(data)
      setError(null)
    } catch (e) {
      setError((e as Error).message)
    }
    setLoading(false)
  }, [id])

  useEffect(() => {
    loadSandbox()
    const interval = setInterval(loadSandbox, 5000)
    return () => clearInterval(interval)
  }, [loadSandbox])

  const [chatLink, setChatLink] = useState<ChatLinkResp | null>(null)
  const [linkBusy, setLinkBusy] = useState(false)

  const connectPhone = useCallback(async () => {
    if (!id) return
    setLinkBusy(true)
    try {
      setChatLink(await createChatLink(id, false))
    } catch (e) {
      const raw = (e as Error).message
      setError(
        /404|not found/i.test(raw)
          ? 'This deployment doesn’t have the messaging bridge — phone linking needs the hosted fleet.'
          : raw,
      )
    }
    setLinkBusy(false)
  }, [id])

  const openUrl = usePreviewLink(sandbox?.id, sandbox?.public_port || 8000)

  const confirmDialog = useConfirm()
  const toast = useToast()
  const doAction = useCallback(async (fn: () => Promise<unknown>, confirmMsg?: string) => {
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
    })
  }, [id, isPrivate, doAction, confirmDialog, toast])

  const [anyChannelUsable, setAnyChannelUsable] = useState(false)
  useEffect(() => {
    if (!id || tplMeta?.kind !== 'agent') return
    let live = true
    getChannels(id)
      .then(r => { if (live) setAnyChannelUsable((r.channels ?? []).some(c => c.available)) })
      .catch(() => {})
    return () => { live = false }
  }, [id, tplMeta?.kind])

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

  const isAgent = agentTabsVisible(tplMeta)

  const tabs: Array<{ id: Tab; label: string }> = [
    ...(isAgent
      ? ([
          { id: 'agent', label: 'Agent' },
          { id: 'timeline', label: 'Timeline' },
          { id: 'connect', label: 'Connect' },
        ] as Array<{ id: Tab; label: string }>)
      : []),
    { id: 'shell', label: 'Terminal' },
    { id: 'files', label: 'Files' },
    { id: 'preview', label: previewLabel(tplMeta?.outcome) },
    { id: 'logs', label: 'Logs' },
    ...(tplMeta?.agent
      ? ([
          { id: 'model', label: 'Model' },
          { id: 'keys', label: 'Keys' },
          { id: 'integrations', label: 'Integrations' },
          ...(anyChannelUsable ? [{ id: 'channels' as Tab, label: 'Channels' }] : []),
        ] as Array<{ id: Tab; label: string }>)
      : []),
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
            <div className="detail-name">{sb.template}</div>
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
              onClick={() => doAction(() => pauseSandbox(id))}
            >
              Pause
            </button>
          ) : (
            <button
              className="btn"
              disabled={busy}
              onClick={() => doAction(() => resumeSandbox(id))}
            >
              Resume
            </button>
          ))}
          {!IS_LOCAL_RUNTIME && <>
          <button
            className="btn"
            disabled={busy}
            title="Copy this sandbox, memory and running processes included"
            onClick={() => doAction(() => forkSandbox(id).then((f) => navigate(`/sandbox/${f.id}`)))}
          >
            Fork
          </button>
          <button
            className="btn"
            disabled={busy}
            title={isPrivate
              ? 'Let everyone in your org use this sandbox (removes the agent logins inside it)'
              : 'Make this sandbox visible only to your API key'}
            onClick={shareToggle}
          >
            {isPrivate ? 'Share with org' : 'Make private'}
          </button>
          </>}
          <button
            className="btn"
            disabled={busy}
            onClick={() => doAction(() => refreshSandbox(id))}
            title="Extend TTL"
          >
            Refresh TTL
          </button>
          {tplMeta?.agent && (
            <button
              className="btn"
              disabled={linkBusy}
              onClick={connectPhone}
              title="Text this workstation from Telegram"
            >
              {linkBusy ? 'Minting…' : 'Connect phone'}
            </button>
          )}
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
        {chatLink && (
          <div style={{ marginTop: 10, display: 'flex', flexDirection: 'column', gap: 6, alignItems: 'flex-start' }}>
            {chatLink.telegram_url ? (
              <a className="btn" href={chatLink.telegram_url} target="_blank" rel="noopener noreferrer">
                Open Telegram ↗
              </a>
            ) : (
              <span style={{ fontSize: 12 }}>
                Send <code>/link {chatLink.code}</code> to your bot.
              </span>
            )}
            <span style={{ fontSize: 12, color: 'var(--sub)' }}>
              Single use, expires in {chatLink.expires_in}. The agent keeps its approval
              prompts on this link — grant autonomy at launch if you want it to act unattended.
            </span>
          </div>
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
        {tab === 'shell' && (
          <ShellTab id={id} status={sb.status} template={sb.template} />
        )}
        {tab === 'files' && <FileBrowser id={id} />}
        {tab === 'preview' && (
          <PreviewTab
            id={id}
            status={sb.status}
            defaultPort={sb.public_port}
          />
        )}
        {tab === 'agent' && isAgent && <AgentPanel sandboxID={id} />}
        {tab === 'timeline' && isAgent && <TimelineTab sandboxID={id} />}
        {tab === 'connect' && isAgent && (
          <>
            <ConnectCard sandboxID={id} />
            <IsolationCard sandboxID={id} />
          </>
        )}
        {tab === 'logs' && <LogsTab id={id} />}
        {tab === 'model' && <ModelTab id={id} />}
        {tab === 'keys' && <KeysTab id={id} />}
        {tab === 'integrations' && <IntegrationsTab id={id} />}
        {tab === 'channels' && <ChannelsTab id={id} />}
        {tab === 'info' && <InfoTab sandbox={sb} onSlug={slug => setSandbox(prev => (prev ? { ...prev, slug: slug || undefined } : prev))} />}
      </div>
    </div>
  )
}

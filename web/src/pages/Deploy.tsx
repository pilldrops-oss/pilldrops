import { useState, useEffect, useMemo, useRef } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { listTemplates, listPublicTemplates, serverOrigin } from '../api'

import type { Template, CreateSandboxReq } from '../types'
import { aiOperatorPrompt } from '../lib/aiPrompt'
import { detectProject } from '../lib/detect'
import { cloneCommand, CLONE_DIR } from '../lib/repo'
import {
  parsePilldropsConfig,
  mergeConfigOverDetection,
  PILLDROPS_FILE,
  type PilldropsConfig,
} from '../lib/pilldropsConfig'

import {
  listRecents,
  recordRecent,
  listPresets,
  savePreset,
  deletePreset,
  listFavorites,
  toggleFavorite,
  timeAgo,
  type StoredLaunch,
  type LaunchPreset,
} from '../lib/launchMemory'
import LazyCodeEditor from '../components/LazyCodeEditor'
import { editorLangFor } from '../components/editorLang'
import {
  TemplateGallery,
  bootableOn,
  PanelHeader,
  SummaryPanel,
  EnvEditor,
  VaultSecretPicker,
  WhatsInside,
  ResourcePicker,
  LifetimePicker,
  DeployProgress,
  deploySteps,
  useLaunchConfig,
  type LaunchConfig,
  buildCliSnippet,
  buildPilldropsExport,
  ExportPilldropsConfig,
  composeCreateBody,
  requiredRuntime,
  startsServer,
  starterFiles,
  sizeLabel,
} from '../components/launchConfig'
import { envPlaceholder } from '../lib/launchKinds'
import { lifetimeToInputs } from '../lib/lifetime'
import { useGitRepo } from '../hooks/useGitRepo'
import { useLaunchRun } from '../hooks/useLaunchRun'
import { useUploadedFiles } from '../hooks/useUploadedFiles'

function AiPromptCard() {
  const [copied, setCopied] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(aiOperatorPrompt())
    } catch {
      const ta = document.createElement('textarea')
      ta.value = aiOperatorPrompt()
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <div className="ai-prompt-row">
      <span className="ai-prompt-row-txt">
        Prefer your own assistant? Copy a prompt for <b>Claude, ChatGPT or Cursor</b> — it
        installs the CLI and provisions for you.
      </span>
      <button
        type="button"
        className={`ai-prompt-row-btn${copied ? ' ai-prompt-copied' : ''}`}
        onClick={copy}
      >
        {copied ? '✓ Copied' : 'Copy prompt'}
      </button>
      <a
        className="ai-prompt-mcp"
        href={`${serverOrigin()}/mcp`}
        onClick={e => e.preventDefault()}
        title={`claude mcp add --transport http pilldrops ${serverOrigin()}/mcp --header "Authorization: Bearer <key>"`}
      >
        or connect via MCP
      </a>
    </div>
  )
}

// Uploaded files win over a git probe; the git tab's findings only apply while
// that tab is the chosen source.
function pickProbeFiles(
  uploaded: Array<{ path: string; content: string }>,
  source: 'upload' | 'git',
  probed: Array<{ path: string; content: string }>,
) {
  if (uploaded.length) return uploaded
  return source === 'git' ? probed : []
}

function setupCommands(composed: string[] | undefined, fromForm: string[]): string[] | undefined {
  if (composed?.length) return composed
  return fromForm.length ? fromForm : undefined
}

function StartConfig({
  tpl,
  start,
  onStartChange,
  port,
  onPortChange,
  previewPort,
}: {
  tpl: Template
  start: string
  onStartChange: (v: string) => void
  port: string
  onPortChange: (v: string) => void
  previewPort: number
}) {
  if (tpl.has_server) {
    return (
      <div className="lm-note">
        <b>Built-in server.</b>{' '}
        {tpl.server_desc || 'This template serves a built-in app on the preview URL.'} Preview
        port: <code>{tpl.default_port ?? tpl.port ?? previewPort}</code>. No start command needed.
      </div>
    )
  }

  return (
    <>
      <div className="form-group lm-fg">
        <label>
          Start command{' '}
          <span className="lm-opt-note">optional — leave blank for just a shell</span>
        </label>
        <input
          value={start}
          onChange={e => onStartChange(e.target.value)}
          placeholder={tpl.start_suggestion || 'e.g. python3 -m http.server $PORT'}
        />
        <p className="lm-help">
          Use <code>$PORT</code> so your app binds the port the preview serves.
        </p>
      </div>
      <div className="form-group lm-fg lm-port">
        <label>Port</label>
        <input
          value={port}
          onChange={e => onPortChange(e.target.value)}
          inputMode="numeric"
          placeholder="8000"
        />
        <p className="lm-help">
          Your app must bind <code>$PORT</code>; the preview serves this port.
        </p>
      </div>
    </>
  )
}

// PurposeConfig renders the per-kind fields the launch form needs beyond
// template defaults: a service's credentials, a GUI's note, a test's command,
// a shell's start command, or (the web fallback) start command + port.
function PurposeConfig({ cfg }: { cfg: LaunchConfig }) {
  const { tpl, kind } = cfg
  if (!tpl) return null

  if (kind === 'service') {
    const svcPort = tpl.default_port ?? tpl.port ?? cfg.previewPort
    const declared = tpl.env ?? []
    return (
      <>
        <div className="lm-sec">
          <div className="lm-sec-h">
            Connection{' '}
            <span className="lm-opt-note">reached at the sandbox host on port {svcPort}</span>
          </div>
          <div className="lm-note">
            This service listens on <b>port {svcPort}</b>. No start command — set the credentials
            below and connect via the sandbox host.
          </div>
        </div>
        {declared.length > 0 && (
          <div className="lm-sec">
            <div className="lm-sec-h">Credentials</div>
            <div className="lm-env">
              {declared.map(e => (
                <label key={e.name} className="lm-env-row">
                  <span className="lm-env-name">
                    {e.name}
                    {e.required && (
                      <span className="lm-req" title="required">
                        {' '}
                        *
                      </span>
                    )}
                  </span>
                  <input
                    type={e.secret ? 'password' : 'text'}
                    value={cfg.declaredEnv[e.name] ?? ''}
                    placeholder={envPlaceholder(e)}
                    autoComplete="off"
                    onChange={ev => cfg.setDeclaredEnvVar(e.name, ev.target.value)}
                  />
                </label>
              ))}
            </div>
          </div>
        )}
      </>
    )
  }

  if (kind === 'gui') {
    return (
      <div className="lm-sec">
        <div className="lm-note">
          <b>Built-in screen.</b>{' '}
          {tpl.server_desc || 'This template streams a graphical desktop on the preview URL.'}{' '}
          Preview port: <code>{tpl.default_port ?? tpl.port ?? cfg.previewPort}</code>. No start
          command — open the preview URL to see and control it.
        </div>
      </div>
    )
  }

  if (kind === 'test') {
    return (
      <>
        <div className="lm-sec">
          <div className="lm-sec-h">
            Test command <span className="lm-opt-note">runs your E2E suite</span>
          </div>
          <input
            className="pa-input mono"
            value={cfg.testCmd}
            onChange={e => cfg.setTestCmd(e.target.value)}
            placeholder="npx playwright test"
          />
          <p className="lm-help">
            Attach your test project (Advanced → Files), then set the command — on finish the HTML
            report is served on the preview URL. Leave blank to just open a terminal.
          </p>
        </div>
        {cfg.tpl?.outcome !== 'report' && (
          <div className="lm-sec">
            <button
              type="button"
              className="pa-toggle"
              aria-pressed={cfg.serveReport}
              onClick={() => cfg.setServeReport(!cfg.serveReport)}
            >
              <span className={`pa-tg${cfg.serveReport ? ' on' : ''}`}>
                <i />
              </span>
              Serve the HTML report on preview
            </button>
          </div>
        )}
      </>
    )
  }

  if (kind === 'shell') {
    return (
      <div className="lm-sec">
        <div className="lm-sec-h">
          Start command{' '}
          <span className="lm-opt-note">optional — leave blank for just a shell</span>
        </div>
        <input
          className="pa-input mono"
          value={cfg.start}
          onChange={e => cfg.setStart(e.target.value)}
          placeholder={tpl.start_suggestion || 'e.g. python3 -m http.server $PORT'}
        />
        <div className="lm-note" style={{ marginTop: 10 }}>
          This is <b>just a shell</b> — a minimal environment. Add a start command to serve
          something on the preview URL, or leave it blank and open an interactive session.
        </div>
      </div>
    )
  }

  return (
    <StartConfig
      tpl={tpl}
      start={cfg.start}
      onStartChange={cfg.setStart}
      port={cfg.port}
      onPortChange={cfg.setPort}
      previewPort={cfg.previewPort}
    />
  )
}

export default function Deploy() {
  const navigate = useNavigate()
  const location = useLocation()
  const wantTemplate =
    (location.state as { templateId?: string } | null)?.templateId ??
    new URLSearchParams(location.search).get('template') ??
    undefined
  const cloneBody = (location.state as { cloneBody?: CreateSandboxReq } | null)?.cloneBody
  const [templates, setTemplates] = useState<Template[]>([])
  const [recents, setRecents] = useState<StoredLaunch[]>(() => listRecents())
  const [presets, setPresets] = useState<LaunchPreset[]>(() => listPresets())
  const [favs, setFavs] = useState<string[]>(() => listFavorites())

  const [filePath, setFilePath] = useState('')
  const [fileContent, setFileContent] = useState('')
  const [starterExtras, setStarterExtras] = useState<Array<{ path: string; content: string }>>([])
  const [seededFor, setSeededFor] = useState<string | null>(null)

  const [codeSource, setCodeSource] = useState<'upload' | 'git'>('upload')
  const [isPublic, setIsPublic] = useState(false)
  const [sandboxName, setSandboxName] = useState('')

  const upload = useUploadedFiles()
  const cfg = useLaunchConfig()
  const launch = useLaunchRun(cfg.kind, (t, body) => {
    recordRecent({ template: t.id, name: t.name || t.id, at: Date.now(), body })
    setRecents(listRecents())
  })

  // Deploy stays mounted across same-route navigations (nav link, "+ New"),
  // so a stale "ready" screen from the last launch would otherwise persist.
  // location.key is fresh on every navigation, including to this same path.
  useEffect(() => {
    launch.reset()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.key])

  useEffect(() => {
    const t = cfg.tpl
    if (!t || t.id === seededFor) return
    if (preserveUploads.current) {
      preserveUploads.current = false
      setSeededFor(t.id)
      return
    }
    const starters = starterFiles(t)
    if (starters.length) {
      setFilePath(starters[0].path)
      setFileContent(starters[0].content)
      setStarterExtras(starters.slice(1))
    } else {
      setFilePath('')
      setFileContent('')
      setStarterExtras([])
    }
    upload.setFiles([])
    upload.setSelected(null)
    upload.setOpened(null)
    setSeededFor(t.id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cfg.tpl])

  const git = useGitRepo(codeSource === 'git')

  const preserveUploads = useRef(false)
  const appliedResourcesFor = useRef<PilldropsConfig['resources'] | undefined>(undefined)
  const probedFiles = useMemo(
    () => pickProbeFiles(upload.files, codeSource, git.files),
    [upload.files, codeSource, git.files],
  )
  const detection = useMemo(
    () => (probedFiles.length ? detectProject(probedFiles) : null),
    [probedFiles],
  )

  const pilldropsResult = useMemo(() => {
    const file = probedFiles.find(f => f.path === PILLDROPS_FILE)
    if (!file) return null
    return parsePilldropsConfig(file.content, templates.map(t => t.id))
  }, [probedFiles, templates])
  const pilldropsConfig: PilldropsConfig | null =
    pilldropsResult && pilldropsResult.ok ? pilldropsResult.config : null
  const pilldropsError = pilldropsResult && !pilldropsResult.ok ? pilldropsResult.error : null

  const merged = useMemo(
    () => mergeConfigOverDetection(pilldropsConfig, detection),
    [pilldropsConfig, detection],
  )

  const [detectionDismissed, setDetectionDismissed] = useState(false)
  useEffect(() => setDetectionDismissed(false), [upload.files])
  const [pilldropsWarningDismissed, setPilldropsWarningDismissed] = useState(false)
  useEffect(() => setPilldropsWarningDismissed(false), [upload.files, git.files])

  useEffect(() => {
    const hasFields = merged.templateId || merged.start || merged.setup || merged.port
    if ((!hasFields && !pilldropsConfig?.resources) || !cfg.tpl) return
    if (merged.templateId && merged.templateId !== cfg.tpl.id) return
    if (merged.start && !cfg.start.trim()) cfg.setStart(merged.start)
    if (merged.setup && !cfg.setup.trim()) cfg.setSetup(merged.setup)
    if (merged.port && !cfg.port.trim()) cfg.setPort(String(merged.port))
    if (pilldropsConfig?.resources && appliedResourcesFor.current !== pilldropsConfig.resources) {
      const { vcpu, memory_mb, storage_mb } = pilldropsConfig.resources
      if (vcpu) cfg.setCpu(vcpu)
      if (memory_mb) cfg.setMem(memory_mb)
      if (storage_mb) cfg.setStorage(storage_mb)
      appliedResourcesFor.current = pilldropsConfig.resources
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [merged, cfg.tpl, pilldropsConfig])

  function acceptDetection() {
    const wantId = merged.templateId
    if (!wantId) return
    const t = templates.find(x => x.id === wantId)
    if (!t) return
    preserveUploads.current = true
    cfg.selectTemplate(t)
  }

  useEffect(() => {
    listTemplates()
      .then(ts => setTemplates(ts))
      .catch(() => listPublicTemplates().then(setTemplates).catch(() => setTemplates([])))
  }, [])

  const bootable = useMemo(() => bootableOn(templates), [templates])

  useEffect(() => {
    if (!wantTemplate || cfg.tpl) return
    const t = templates.find(x => x.id === wantTemplate)
    if (t) { cfg.selectTemplate(t); window.scrollTo(0, 0) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [templates, wantTemplate])

  const cloneApplied = useRef(false)
  useEffect(() => {
    if (!cloneBody || !cfg.tpl || cloneApplied.current) return
    cloneApplied.current = true
    if (cloneBody.start !== undefined) cfg.setStart(cloneBody.start)
    if (cloneBody.setup?.length) cfg.setSetup(cloneBody.setup.join('\n'))
    if (cloneBody.port !== undefined) cfg.setPort(String(cloneBody.port))
    if (cloneBody.vcpu) cfg.setCpu(cloneBody.vcpu)
    if (cloneBody.memory_mb) cfg.setMem(cloneBody.memory_mb)
    if (cloneBody.storage_mb) cfg.setStorage(cloneBody.storage_mb)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cloneBody, cfg.tpl])

  const effLifetime = useMemo(() => {
    if (cfg.timeout.trim() !== '' || cfg.idle.trim() !== '') {
      return { timeoutInput: cfg.timeout, idleInput: cfg.idle }
    }
    return lifetimeToInputs(cfg.lifetime)
  }, [cfg.timeout, cfg.idle, cfg.lifetime])

  const composed = useMemo(
    () =>
      cfg.tpl
        ? composeCreateBody({
            tpl: cfg.tpl,
            kind: cfg.kind,
            testCmd: cfg.testCmd,
            serveReport: cfg.serveReport,
            start: cfg.start,
            port: cfg.port,
            envList: cfg.envList,
            secretRefs: cfg.secretRefs,
            previewPort: cfg.previewPort,
            timeoutInput: effLifetime.timeoutInput,
            idleInput: effLifetime.idleInput,
          })
        : null,
    [
      cfg.tpl,
      cfg.kind,
      cfg.testCmd,
      cfg.serveReport,
      cfg.start,
      cfg.port,
      cfg.envList,
      cfg.secretRefs,
      cfg.previewPort,
      effLifetime,
    ],
  )

  const cliSnippet = useMemo(
    () =>
      buildCliSnippet({
        tpl: cfg.tpl,
        start: cfg.start,
        port: cfg.port,
        envList: cfg.envList,
        cpu: cfg.cpu,
        mem: cfg.mem,
        isPublic,
        composed: composed ?? undefined,
      }),
    [cfg.tpl, cfg.start, cfg.port, cfg.envList, cfg.cpu, cfg.mem, isPublic, composed],
  )

  const pilldropsExportConfig = useMemo(
    () =>
      buildPilldropsExport({
        tpl: cfg.tpl,
        cpu: cfg.cpu,
        mem: cfg.mem,
        storage: cfg.storage,
        start: cfg.start,
        setupCmds: cfg.setupCmds,
        port: cfg.port,
        previewPort: cfg.previewPort,
        customEnv: cfg.customEnv,
      }),
    [cfg.tpl, cfg.cpu, cfg.mem, cfg.storage, cfg.start, cfg.setupCmds, cfg.port, cfg.previewPort, cfg.customEnv],
  )

  const files = useMemo(() => {
    if (codeSource === 'git' && git.repo) {
      return [...(composed?.files ?? [])]
    }
    if (upload.files.length > 0) {
      return [...(composed?.files ?? []), ...upload.files]
    }
    const primary = filePath && fileContent ? [{ path: filePath, content: fileContent }] : []
    return [...(composed?.files ?? []), ...primary, ...starterExtras]
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [composed, filePath, fileContent, starterExtras, upload.files, codeSource, git.url])

  const steps = useMemo(
    () =>
      deploySteps({
        kind: cfg.kind,
        hasFiles: files.length > 0,
        hasSetup: !!(composed?.setup?.length || cfg.setupCmds.length),
        hasStart: !!composed?.start,
      }),
    [cfg.kind, files.length, composed, cfg.setupCmds.length],
  )

  function pickTemplate(t: Template) {
    cfg.selectTemplate(t)
    setIsPublic(false)
    launch.reset()
    window.scrollTo(0, 0)
  }

  function backToGallery() {
    cfg.clearTemplate()
    launch.reset()
    window.scrollTo(0, 0)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    await submitDeploy()
  }

  const [autoLaunch, setAutoLaunch] = useState(false)
  function quickLaunch(t: Template, primaryValue?: string) {
    pickTemplate(t)
    if (primaryValue) cfg.setPrimaryValue(primaryValue)
    setAutoLaunch(true)
  }
  useEffect(() => {
    if (!autoLaunch || !cfg.tpl || !composed || launch.phase !== 'form') return
    if (seededFor !== cfg.tpl.id) return
    setAutoLaunch(false)
    void submitDeploy()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoLaunch, cfg.tpl, composed, launch.phase, seededFor])

  function buildRequestBody(): CreateSandboxReq | null {
    if (!cfg.tpl || !composed) return null
    const body: CreateSandboxReq = {
      template: cfg.tpl.id,
      name: sandboxName.trim() || undefined,
      vcpu: cfg.cpu,
      memory_mb: cfg.mem,
      storage_mb: cfg.storage,
      runtime: requiredRuntime(cfg.tpl),
      start: composed.start,
      port: composed.port,
      env: composed.env,
      secret_refs: composed.secret_refs,
      setup: setupCommands(composed.setup, cfg.setupCmds),
      files: files.length ? files : undefined,
      timeout: composed.timeout,
      idle_timeout: composed.idle_timeout,
      public: isPublic || undefined,
    }
    if (codeSource === 'git' && git.repo) {
      body.setup = [cloneCommand(git.repo, git.branch), ...(body.setup ?? [])]
      const userStart = cfg.start.trim()
      if (userStart) body.start = `cd ${CLONE_DIR} && ${userStart}`
    }
    return body
  }

  async function submitDeploy() {
    const body = buildRequestBody()
    if (!body || !cfg.tpl) return
    await launch.run(body, cfg.tpl, cfg.tpl.default_port)
  }

  function relaunch(entry: StoredLaunch) {
    const t = templates.find(x => x.id === entry.template)
    if (!t) return
    if (!entry.body) {
      pickTemplate(t)
      return
    }
    cfg.selectTemplate(t)
    window.scrollTo(0, 0)
    void launch.run(entry.body, t, t.default_port)
  }

  const [aiPlanPendingLaunch, setAiPlanPendingLaunch] =
    useState<{ templateId: string; start: string } | null>(null)

  useEffect(() => {
    if (!aiPlanPendingLaunch || !cfg.tpl || launch.phase !== 'form') return
    if (cfg.tpl.id !== aiPlanPendingLaunch.templateId) return
    if (cfg.start !== aiPlanPendingLaunch.start) return
    setAiPlanPendingLaunch(null)
    void submitDeploy()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [aiPlanPendingLaunch, cfg.tpl, cfg.start, launch.phase])

  function savePresetNow() {
    const body = buildRequestBody()
    if (!body || !cfg.tpl) return
    const label = window.prompt('Preset name', `${cfg.tpl.name || cfg.tpl.id} preset`)
    if (!label?.trim()) return
    savePreset({
      label: label.trim(),
      template: cfg.tpl.id,
      name: cfg.tpl.name || cfg.tpl.id,
      at: Date.now(),
      body,
    })
    setPresets(listPresets())
  }

  function removePreset(label: string) {
    deletePreset(label)
    setPresets(listPresets())
  }

  const tpl = cfg.tpl

  const codeIsPrimary = cfg.kind === 'web' || cfg.kind === 'shell'
  const hasStarter = !!(fileContent && seededFor === tpl?.id)

  const hasUpload = upload.files.length > 0
  const selFileObj = upload.files.find(f => f.path === upload.selected) ?? upload.files[0]
  const openFileObj = upload.files.find(f => f.path === upload.opened)

  const onPick = (e: React.ChangeEvent<HTMLInputElement>) => { upload.add(e.target.files); e.target.value = '' }
  const uploadButtons = (
    <div className="dep-drop-actions">
      <label className="dep-upload-btn">
        Browse files
        <input type="file" multiple hidden onChange={onPick} />
      </label>
      <label className="dep-upload-btn">
        Choose a .zip
        <input type="file" accept=".zip,application/zip" hidden onChange={onPick} />
      </label>
      <label className="dep-upload-btn">
        Upload a folder
        {/* @ts-expect-error non-standard but widely supported directory picker */}
        <input type="file" hidden webkitdirectory="" directory="" multiple onChange={onPick} />
      </label>
    </div>
  )

  const codeEditor = (
    <>
      {hasUpload ? (
        <>
        <div
          className={`dep-drop dep-drop-sm${upload.dragOver ? ' over' : ''}`}
          onDragOver={e => { e.preventDefault(); upload.setDragOver(true) }}
          onDragLeave={() => upload.setDragOver(false)}
          onDrop={upload.onDrop}
        >
          <span className="dep-drop-big">Drop more files, or</span>
          {uploadButtons}
        </div>
        <div className="dep-proj">
          <div className="dep-proj-head">
            <span>Uploaded project · {upload.files.length} file{upload.files.length === 1 ? '' : 's'}</span>
            <button type="button" className="dep-filechip-clear" onClick={upload.clear}>clear &amp; use starter</button>
          </div>
          <div className="dep-proj-body">
            <div className="dep-proj-files">
              {upload.files.map(f => (
                <div
                  key={f.path}
                  className={`dep-proj-file${selFileObj?.path === f.path ? ' on' : ''}`}
                  onClick={() => upload.setSelected(f.path)}
                  onDoubleClick={() => upload.setOpened(f.path)}
                >
                  <span className="nm">{f.path}</span>
                  <button
                    type="button"
                    className="rm"
                    aria-label={`remove ${f.path}`}
                    onClick={e => {
                      e.stopPropagation()
                      const rest = upload.files.filter(x => x.path !== f.path)
                      upload.setFiles(rest)
                      if (upload.selected === f.path) upload.setSelected(rest[0]?.path ?? null)
                    }}
                  >×</button>
                </div>
              ))}
            </div>
            <div className="dep-proj-preview">
              {selFileObj ? (
                <>
                  <div className="dep-proj-pv-head">
                    <span className="path">{selFileObj.path}</span>
                    <button type="button" className="dep-proj-open" onClick={() => upload.setOpened(selFileObj.path)}>
                      Open in editor ↗
                    </button>
                  </div>
                  <pre className="dep-proj-code" onClick={() => upload.setOpened(selFileObj.path)}>
                    {selFileObj.content.slice(0, 4000)}
                    {selFileObj.content.length > 4000 ? '\n…' : ''}
                  </pre>
                </>
              ) : (
                <div className="dep-proj-empty">Select a file to preview.</div>
              )}
            </div>
          </div>
          <p className="lm-help">
            Click a file to preview it, or <b>Open in editor</b> to edit full screen. Deployed
            with <code>{cfg.start || tpl?.start_suggestion}</code>.
          </p>
        </div>
        </>
      ) : (
        <>
          {hasStarter && (
            <p className="lm-hint" style={{ marginTop: 0 }}>
              A working starter is loaded so your preview runs right away — upload your own project,
              or edit the starter below. Launched with <code>{cfg.start || tpl?.start_suggestion}</code>.
            </p>
          )}

          <div
            className={`dep-drop dep-drop-tall${upload.dragOver ? ' over' : ''}`}
            style={{ marginTop: hasStarter ? 12 : 0 }}
            onDragOver={e => { e.preventDefault(); upload.setDragOver(true) }}
            onDragLeave={() => upload.setDragOver(false)}
            onDrop={upload.onDrop}
          >
            <div className="dep-drop-big">
              Drop a <span className="dep-drop-zip">.zip</span>, files or a folder here
            </div>
            <div className="dep-drop-sub">
              a whole project keeps its structure · we skip node_modules, .git &amp; build output
            </div>
            {uploadButtons}
          </div>

          <div className="dep-or"><span>or edit a single file</span></div>
          <label className="lm-adv-label" htmlFor="filepath">
            Main file <span className="lm-opt-note">the entry point your start command runs</span>
          </label>
          <input
            id="filepath"
            className="dep-input"
            type="text"
            placeholder="server.js"
            value={filePath}
            onChange={e => setFilePath(e.target.value)}
          />
          <label className="lm-adv-label" htmlFor="filecontent">File content</label>
          <textarea
            id="filecontent"
            className="lm-setup"
            placeholder="# Paste your application code here"
            value={fileContent}
            onChange={e => setFileContent(e.target.value)}
            style={{ minHeight: 180, fontFamily: 'var(--mono)' }}
          />
          {starterExtras.length > 0 && (
            <div className="dep-filechips">
              {starterExtras.map(f => (
                <span key={f.path} className="dep-filechip" title="starter support file">{f.path}</span>
              ))}
            </div>
          )}
        </>
      )}

      {upload.opened && openFileObj && (
        <div className="dep-editor-full" role="dialog" aria-modal="true">
          <div className="dep-editor-bar">
            <span className="path">{upload.opened}</span>
            <button type="button" className="dep-editor-close" onClick={() => upload.setOpened(null)}>Done ✕</button>
          </div>
          <div className="dep-editor-cm">
            <LazyCodeEditor
              value={openFileObj.content}
              onChange={v => upload.edit(openFileObj.path, v)}
              language={editorLangFor(openFileObj.path)}
              minHeight={0}
              maxHeight={100000}
            />
          </div>
        </div>
      )}
    </>
  )

  const gitSource = (
    <div className="dep-git">
      <div className="dep-git-hero">
        <span className="dep-git-ic" aria-hidden="true">
          <svg width="26" height="26" viewBox="0 0 24 24" fill="currentColor"><path d="M12 .3a12 12 0 0 0-3.8 23.4c.6.1.8-.3.8-.6v-2c-3.3.7-4-1.6-4-1.6-.6-1.4-1.3-1.8-1.3-1.8-1-.7.1-.7.1-.7 1.2 0 1.8 1.2 1.8 1.2 1 1.8 2.8 1.3 3.5 1 0-.8.4-1.3.7-1.6-2.7-.3-5.5-1.3-5.5-6 0-1.2.5-2.3 1.3-3.1-.2-.4-.6-1.6 0-3.2 0 0 1-.3 3.4 1.2a11.5 11.5 0 0 1 6 0c2.3-1.5 3.3-1.2 3.3-1.2.6 1.6.2 2.8 0 3.2.9.8 1.3 1.9 1.3 3.2 0 4.6-2.8 5.6-5.5 5.9.5.4.9 1 .9 2.2v3.3c0 .3.1.7.8.6A12 12 0 0 0 12 .3"/></svg>
        </span>
        <h3>Deploy a public GitHub repo</h3>
        <p>
          Paste a repo URL — the sandbox clones it at boot (into <code>app/</code>) and your start
          command runs inside it. Detection reads the repo's manifests and pre-fills the rest.
        </p>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginTop: 10 }}>
          <input
            className="dep-quick-in"
            style={{ flex: '2 1 260px' }}
            placeholder="https://github.com/owner/repo (or owner/repo)"
            value={git.url}
            onChange={e => git.setUrl(e.target.value)}
            aria-label="Public GitHub repository URL"
          />
          <input
            className="dep-quick-in"
            style={{ flex: '1 1 120px' }}
            placeholder="branch (default)"
            value={git.branch}
            onChange={e => git.setBranch(e.target.value)}
            aria-label="Branch"
          />
          <button
            type="button"
            className="dep-quick-btn"
            disabled={!git.repo || git.probe === 'checking'}
            onClick={() => void git.runProbe()}
          >
            {git.probe === 'checking' ? 'Checking…' : 'Detect'}
          </button>
        </div>
        {git.url.trim() && !git.repo && (
          <p className="lm-hint pa-warn">
            That doesn't look like a public GitHub repo URL (https://github.com/owner/repo).
          </p>
        )}
        {git.probe === 'ok' && (
          <p className="lm-help">
            ✓ Repo reachable — {git.files.length} manifest file
            {git.files.length === 1 ? '' : 's'} read; start/setup pre-filled below where
            detection matched.
          </p>
        )}
        {git.probe === 'none' && (
          <p className="lm-hint pa-warn">
            Couldn't read manifest files — private repo, wrong branch, or no recognizable project
            at the root. You can still deploy; set the start command yourself.
          </p>
        )}
        <p className="lm-opt-note" style={{ marginTop: 8 }}>
          Private repos and deploy-on-push need the GitHub app — coming later.
        </p>
      </div>
    </div>
  )

  return (
    <div className={`wrap${tpl && launch.phase === 'form' ? ' dep-wide' : ''}`}>
      {!tpl ? (
        <>
          <div className="h1row">
            <div>
              <h1 className="h1">Catalog</h1>
              <p>
                Pick a template to start — pre-configured environments for apps and testing.
              </p>
            </div>
          </div>
          <>
              <AiPromptCard />
              {(presets.length > 0 || recents.length > 0) && (
                <div className="dep-rail" aria-label="Presets and recent launches">
                  {presets.map(p => (
                    <span key={`p-${p.label}`} className="dep-rail-chip">
                      <button
                        type="button"
                        className="dep-rail-run"
                        title={`Launch preset “${p.label}”`}
                        onClick={() => relaunch(p)}
                      >
                        ★ {p.label}
                      </button>
                      <button
                        type="button"
                        className="dep-rail-x"
                        aria-label={`Delete preset ${p.label}`}
                        onClick={() => removePreset(p.label)}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                  {recents.map(r => (
                    <span key={`r-${r.template}`} className="dep-rail-chip">
                      <button
                        type="button"
                        className="dep-rail-run"
                        title={r.body ? 'Relaunch with the same configuration' : 'Open pre-configured'}
                        onClick={() => relaunch(r)}
                      >
                        ↻ {r.name}
                        <span className="dep-rail-ago"> · {timeAgo(r.at)}</span>
                      </button>
                    </span>
                  ))}
                </div>
              )}
              <TemplateGallery
                templates={templates}
                onPick={pickTemplate}
                onQuickLaunch={quickLaunch}
                favorites={favs}
                onToggleFavorite={id => setFavs(toggleFavorite(id))}
                bootable={bootable}
              />
            </>
        </>
      ) : (
        <>
          <form onSubmit={handleSubmit} className="dep-panel">
            <PanelHeader tpl={tpl} onBack={launch.phase === 'running' ? undefined : backToGallery} />

            {launch.phase !== 'form' ? (
              <div className="dep-pb dep-pb-progress">
                <DeployProgress
                  steps={steps}
                  state={launch.phase}
                  verifying={launch.verifying}
                  previewOutcome={launch.previewOutcome}
                  liveStatus={launch.liveStatus}
                  onCancel={launch.phase === 'running' ? launch.cancel : undefined}
                  onRetry={
                    launch.lastAttempt
                      ? () => void launch.run(launch.lastAttempt!.body, launch.lastAttempt!.template)
                      : undefined
                  }
                  result={launch.result}
                  error={launch.error}
                  onReset={launch.reset}
                  service={cfg.kind === 'service' ? tpl : null}
                  onOpen={id => navigate(`/sandbox/${id}`, { state: { sandbox: launch.result } })}
                />
              </div>
            ) : (
            <div className="dep-pb">
              <div className="dep-config">
                <WhatsInside tpl={tpl} />

                {(
                  <div className="dep-tabpanel">
                    {codeIsPrimary && (
                      <div className="dep-srcrow">
                        <span className="dep-srclbl">Code source</span>
                        <div className="dep-seg">
                          <button type="button" className={`dep-segbtn${codeSource === 'upload' ? ' on' : ''}`}
                            onClick={() => setCodeSource('upload')}>Upload / paste</button>
                          <button type="button" className={`dep-segbtn${codeSource === 'git' ? ' on' : ''}`}
                            onClick={() => setCodeSource('git')}>From GitHub <span className="dep-soon">soon</span></button>
                        </div>
                      </div>
                    )}
                    {(() => {
                      const pilldropsWarning = pilldropsError &&
                        !pilldropsWarningDismissed && (
                          <div className="dep-detect dep-cfgwarn" role="alert">
                            <span className="dep-detect-msg">⚠️ {pilldropsError}</span>
                            <button
                              type="button"
                              className="dep-rail-x"
                              aria-label="Dismiss warning"
                              onClick={() => setPilldropsWarningDismissed(true)}
                            >
                              ×
                            </button>
                          </div>
                        )
                      function detectionDetail() {
                        if (merged.templateId && merged.templateId !== tpl?.id) {
                          const better = templates.find(x => x.id === merged.templateId)?.name ?? merged.templateId
                          return (
                            <>
                              {' '}— runs best on the <b>{better}</b> template
                              {merged.source.templateId === 'pilldrops.json' && (
                                <> (from <code>{PILLDROPS_FILE}</code>)</>
                              )}
                              .
                            </>
                          )
                        }
                        if (merged.start) {
                          return (
                            <>
                              {' '}— start command pre-filled
                              {merged.source.start === 'pilldrops.json' && (
                                <> from <code>{PILLDROPS_FILE}</code></>
                              )}
                              .
                            </>
                          )
                        }
                        return null
                      }

                      const detectionBanner = (merged.templateId || merged.start || merged.setup || merged.port) &&
                        !detectionDismissed &&
                        (upload.files.length > 0 || git.files.length > 0) && (
                          <div className="dep-detect" role="status">
                            <span className="dep-detect-msg">
                              {detection?.label ? (
                                <>🔎 Detected <b>{detection.label}</b></>
                              ) : (
                                <>📄 Configuration found</>
                              )}
                              {detectionDetail()}
                            </span>
                            {merged.templateId &&
                              merged.templateId !== tpl.id &&
                              templates.some(x => x.id === merged.templateId) && (
                                <button type="button" className="btn" onClick={acceptDetection}>
                                  Switch (keeps your files)
                                </button>
                              )}
                            <button
                              type="button"
                              className="dep-rail-x"
                              aria-label="Dismiss detection"
                              onClick={() => setDetectionDismissed(true)}
                            >
                              ×
                            </button>
                          </div>
                        )
                      return codeIsPrimary && codeSource === 'git' ? (
                        <>
                          {gitSource}
                          {pilldropsWarning}
                          {detectionBanner}
                        </>
                      ) : (
                        <>
                          <PurposeConfig cfg={cfg} />
                          {pilldropsWarning}
                          {detectionBanner}
                          {codeIsPrimary && <div className="lm-sec">{codeEditor}</div>}
                        </>
                      )
                    })()}
                  </div>
                )}

                {cfg.kind !== 'service' && (
                  <details className="dep-sec" open={!!tpl.env?.length}>
                    <summary className="dep-sec-sum">
                      Environment variables{' '}
                      <span className="lm-opt-note">
                        {tpl.env?.length ? 'declared by this template + your own' : 'optional — NAME=VALUE pairs'}
                      </span>
                    </summary>
                    <div className="dep-sec-body">
                      <EnvEditor
                        declared={tpl.env ?? []}
                        values={cfg.declaredEnv}
                        onDeclaredChange={cfg.setDeclaredEnvVar}
                        custom={cfg.customEnv}
                        onCustomChange={cfg.setCustomEnv}
                      />
                      <VaultSecretPicker selected={cfg.secretRefs} onChange={cfg.setSecretRefs} />
                      <p className="lm-help">
                        Injected into your app and terminal at boot — never shown in the preview URL or logs.
                      </p>
                    </div>
                  </details>
                )}

                <details className="dep-sec">
                  <summary className="dep-sec-sum">
                    Machine{' '}
                    <span className="lm-opt-note">
                      {cfg.cpu} vCPU · {sizeLabel(cfg.mem)} · {sizeLabel(cfg.storage)}
                    </span>
                  </summary>
                  <div className="dep-sec-body">
                    <ResourcePicker
                      cpu={cfg.cpu}
                      mem={cfg.mem}
                      storage={cfg.storage}
                      onCpu={cfg.setCpu}
                      onMem={cfg.setMem}
                      onStorage={cfg.setStorage}
                      plan={cfg.plan}
                    />
                  </div>
                </details>

                <details className="dep-sec">
                  <summary className="dep-sec-sum">
                    Advanced{' '}
                    <span className="lm-opt-note">
                      name · setup commands · lifetime · visibility
                      {!codeIsPrimary ? ' · files' : ''}
                    </span>
                  </summary>
                  <div className="dep-sec-body">
                    <label className="lm-adv-label" htmlFor="dep-sandbox-name">
                      Name <span className="lm-opt-note">optional — shown in the sandbox list instead of the id</span>
                    </label>
                    <input
                      id="dep-sandbox-name"
                      className="dep-quick-in"
                      style={{ display: 'block', width: '100%', marginBottom: 14 }}
                      value={sandboxName}
                      onChange={e => setSandboxName(e.target.value)}
                      maxLength={64}
                      placeholder="e.g. staging-worker"
                    />
                    <label className="lm-adv-label">
                      Setup commands <span className="lm-opt-note">one per line, run once before start</span>
                    </label>
                    <textarea
                      className="lm-setup"
                      value={cfg.setup}
                      onChange={e => cfg.setSetup(e.target.value)}
                      placeholder={'one command per line\ne.g. pip install fastapi uvicorn'}
                    />
                    <div style={{ marginTop: 14 }}>
                      <LifetimePicker value={cfg.lifetime} onChange={cfg.setLifetime} />
                      {startsServer(cfg.kind, cfg.start) &&
                        cfg.lifetime === 'idle' &&
                        !cfg.timeout.trim() &&
                        !cfg.idle.trim() && (
                          <p className="lm-hint">
                            This template serves a preview, so it stays alive (never idle-pauses,
                            1h max) instead of pausing right away — pick a different lifetime
                            above to change that.
                          </p>
                        )}
                      <details className="lm-adv-exact" style={{ marginTop: 10 }}>
                        <summary className="lm-linkbtn" style={{ cursor: 'pointer' }}>
                          Set an exact value in seconds instead
                        </summary>
                        <div className="lm-res" style={{ marginTop: 10 }}>
                          <label className="lm-res-item">
                            Timeout (s)
                            <input
                              value={cfg.timeout}
                              onChange={e => cfg.setTimeout(e.target.value)}
                              inputMode="numeric"
                              placeholder={startsServer(cfg.kind, cfg.start) ? '3600' : '900'}
                            />
                          </label>
                          <label className="lm-res-item">
                            Idle (s) · -1 = never
                            <input
                              value={cfg.idle}
                              onChange={e => cfg.setIdle(e.target.value)}
                              inputMode="numeric"
                              placeholder={startsServer(cfg.kind, cfg.start) ? '-1 (never)' : '300'}
                            />
                          </label>
                        </div>
                        <p className="lm-help">
                          Overrides the lifetime choice above. Timeout = max lifetime. Idle =
                          auto-pause after inactivity; <code>-1</code> = never pause.
                        </p>
                      </details>
                    </div>
                    <div className="lm-sec" style={{ marginTop: 14 }}>
                      <div className="lm-sec-h">Preview visibility</div>
                      <label className="lm-check" style={{ display: 'flex', gap: 10, alignItems: 'flex-start', cursor: 'pointer' }}>
                        <input
                          type="checkbox"
                          checked={isPublic}
                          onChange={e => setIsPublic(e.target.checked)}
                          style={{ marginTop: 3 }}
                        />
                        <span>
                          <b>Public preview</b> — anyone with the URL can open it (no token).
                          <span className="lm-opt-note" style={{ display: 'block' }}>
                            Default is private: the preview URL carries an access token; the bare
                            URL returns 403. Enable this only for a link you intend to share.
                          </span>
                        </span>
                      </label>
                    </div>
                    {!codeIsPrimary && (
                      <div className="lm-sec">
                        <div className="lm-sec-h">
                          Files <span className="lm-opt-note">optional — attach code to this sandbox</span>
                        </div>
                        {codeEditor}
                      </div>
                    )}
                  </div>
                </details>
              </div>

              <SummaryPanel
                tpl={tpl}
                kind={cfg.kind}
                cpu={cfg.cpu}
                mem={cfg.mem}
                previewPort={cfg.previewPort}
                previewUrl={cfg.previewUrl}
                cliSnippet={cliSnippet}
                busy={launch.phase !== 'form'}
                ctaDisabledNote={
                  tpl.kind === 'verdict' &&
                  tpl.primary_input?.required &&
                  !cfg.primaryValue.trim() &&
                  !(tpl.primary_input.type === 'target' && files.length > 0)
                    ? `Enter ${tpl.primary_input.label.toLowerCase()} first — it's what this template runs against.`
                    : undefined
                }
              />
            </div>
            )}
          </form>

          {launch.phase === 'form' && (
            <div className="dep-cancel">
              <button type="button" className="btn ghost" onClick={() => navigate('/')}>
                Cancel
              </button>
              <button
                type="button"
                className="btn ghost"
                title="Save this configuration as a named preset (shown above the gallery)"
                onClick={savePresetNow}
              >
                ☆ Save as preset
              </button>
              <ExportPilldropsConfig config={pilldropsExportConfig} />
            </div>
          )}
        </>
      )}
    </div>
  )
}

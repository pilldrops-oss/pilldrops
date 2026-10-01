import type { Template } from '../types'
import { PILLDROPS_VERSION, type PilldropsConfig } from './pilldropsConfig'
import { type ComposedBody, type CustomEnv, type Kind, shellQuote, startsServer, requiredRuntime, resolveLifetime } from './launchKinds'
import { START_FILE, universalStartBody } from './launchStarters'

// Verdict templates run their check on boot; the target arrives as $PD_TARGET_URL.
const VERDICT_RUNNER: Record<string, string> = {
  loadtest: 'pdload "$PD_TARGET_URL"',
  security: 'pdscan --url "$PD_TARGET_URL"',
}

// composeCreateBody assembles the per-kind create-request slice from the form state.
export function composeCreateBody(cfg: {
  tpl: Template
  kind: Kind
  testCmd: string
  serveReport: boolean
  start: string
  port: string
  envList: string[]
  previewPort: number
  /** Seconds; empty means keep-alive default. */
  timeoutInput?: string
  /** Seconds; empty means keep-alive default. */
  idleInput?: string
  /** The manifest primary input's value (verdict URL/target). */
  primaryValue?: string
}): ComposedBody {
  const {
    tpl,
    kind,
    testCmd,
    serveReport,
    start,
    port,
    envList,
    previewPort,
    timeoutInput = '',
    idleInput = '',
    primaryValue = '',
  } = cfg
  const portNum = port ? parseInt(port, 10) : undefined
  const life = resolveLifetime(startsServer(kind, start), timeoutInput, idleInput)

  // Verdict templates run their check on boot. The target travels as an env var
  // so user input is never shell-quoted; a blank target leaves the built-in server.
  const verdictRunner =
    tpl.kind === 'verdict' && primaryValue.trim() ? (VERDICT_RUNNER[tpl.id] ?? '') : ''
  if (verdictRunner) {
    return {
      env: [...envList, `PD_TARGET_URL=${primaryValue.trim()}`],
      start: `sh -c '${verdictRunner}; exec /usr/local/bin/start.sh'`,
      port: portNum ?? previewPort,
      ...resolveLifetime(true, timeoutInput, idleInput),
    }
  }

  if (kind === 'service') {
    return { env: envList.length ? envList : undefined, start: undefined, port: portNum, ...life }
  }

  if (kind === 'gui') {
    return { env: envList.length ? envList : undefined, start: undefined, port: portNum, ...life }
  }

  if (kind === 'test') {
    const cmd = testCmd.trim()
    if (cmd) {
      // `;` not `&&`: the report must still serve when tests fail.
      const reportServe = /playwright/.test(cmd)
        ? 'npx playwright show-report --host 0.0.0.0 --port $PORT'
        : 'python3 -m http.server $PORT'
      const start = serveReport ? `${cmd}; ${reportServe}` : cmd
      return { env: envList.length ? envList : undefined, start, port: portNum, ...life }
    }
    const body = universalStartBody(previewPort)
    const kal = resolveLifetime(true, timeoutInput, idleInput)
    return { env: envList.length ? envList : undefined, ...body, ...kal }
  }

  if (kind === 'shell') {
    if (start.trim()) {
      return {
        env: envList.length ? envList : undefined,
        start: start.trim(),
        port: portNum,
        ...life,
      }
    }
    const body = universalStartBody(previewPort)
    const kal = resolveLifetime(true, timeoutInput, idleInput)
    return { env: envList.length ? envList : undefined, ...body, ...kal }
  }

  // web: no start command and no built-in server gets a browser terminal so the preview never hangs.
  if (!tpl.has_server && !start.trim()) {
    const body = universalStartBody(previewPort)
    const kal = resolveLifetime(true, timeoutInput, idleInput)
    return { env: envList.length ? envList : undefined, ...body, ...kal }
  }
  return {
    env: envList.length ? envList : undefined,
    start: tpl.has_server ? undefined : start.trim() || undefined,
    port: portNum,
    ...life,
  }
}

// Env names only, never values; the export must not leak secrets.
function exportedEnvNames(tpl: Template | null, customEnv: CustomEnv[]): string[] {
  const declared = (tpl?.env ?? []).map(e => e.name)
  const custom = customEnv.map(row => row.name.trim()).filter(Boolean)
  return Array.from(new Set([...declared, ...custom]))
}

export function buildPilldropsExport(cfg: {
  tpl: Template | null
  cpu: number
  mem: number
  storage: number
  start: string
  setupCmds: string[]
  port: string
  previewPort: number
  customEnv: CustomEnv[]
}): PilldropsConfig {
  const config: PilldropsConfig = { version: PILLDROPS_VERSION }
  if (cfg.tpl) config.template = cfg.tpl.id

  const resources: NonNullable<PilldropsConfig['resources']> = {}
  if (cfg.cpu) resources.vcpu = cfg.cpu
  if (cfg.mem) resources.memory_mb = cfg.mem
  if (cfg.storage) resources.storage_mb = cfg.storage
  if (Object.keys(resources).length) config.resources = resources

  if (cfg.setupCmds.length) config.setup = cfg.setupCmds
  if (cfg.start.trim()) config.start = cfg.start.trim()

  const port = cfg.port.trim() ? parseInt(cfg.port, 10) : cfg.previewPort
  if (port) config.port = port

  const env = exportedEnvNames(cfg.tpl, cfg.customEnv)
  if (env.length) config.env = env

  return config
}

// buildCliSnippet mirrors the current config as a CLI command.
export function buildCliSnippet(cfg: {
  tpl: Template | null
  start: string
  port: string
  envList: string[]
  /** When present it drives the flags so the snippet reflects the real request. */
  composed?: ComposedBody
}): string {
  const { tpl, start, port, envList, composed } = cfg
  if (!tpl) return ''

  const parts = ['pilldrops sandbox create', `--template ${tpl.id}`]
  const rt = requiredRuntime(tpl)
  if (rt) parts.push(`--runtime ${rt}`)

  let startCmd: string | undefined
  if (composed) startCmd = composed.start
  else startCmd = tpl.has_server ? '' : start.trim()
  const envs = composed ? composed.env ?? [] : envList
  let portVal = port
  if (composed) portVal = composed.port ? String(composed.port) : ''
  const hasUniversalScript = !!composed?.files?.some(f => f.path === START_FILE)

  for (const kv of envs) {
    // Mask secret-looking values but keep base URL and model visible.
    const [name, ...rest] = kv.split('=')
    const val = rest.join('=')
    const masked = /key|token|secret|password/i.test(name) && val ? `${name}=***` : kv
    parts.push(`--env ${shellQuote(masked)}`)
  }
  for (const s of composed?.setup ?? []) parts.push(`--setup ${shellQuote(s)}`)
  if (startCmd) parts.push(`--start ${shellQuote(startCmd)}`)
  if (portVal) parts.push(`--port ${portVal}`)
  if (composed?.timeout !== undefined) parts.push(`--timeout ${composed.timeout}`)
  if (composed?.idle_timeout !== undefined) parts.push(`--idle-timeout ${composed.idle_timeout}`)

  let out = parts.join(' \\\n  ')
  if (hasUniversalScript) {
    out += `\n  # terminal script written to ${START_FILE} (serves a browser terminal on the preview)`
  }
  return out
}

// genSecret mints a URL-safe random credential; the alphabet omits look-alikes (0/O, 1/l/I).
export function genSecret(len = 24): string {
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789'
  const buf = new Uint32Array(len)
  crypto.getRandomValues(buf)
  return Array.from(buf, n => alphabet[n % alphabet.length]).join('')
}

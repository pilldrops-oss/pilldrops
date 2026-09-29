import type { Template, TemplateEnv } from '../types'

import { AGENT_CONFIG, type AgentConfig } from './launchAgents'
import { sandboxHost } from '../api'

// Kind is a template's purpose; it drives the config body, summary and CTA.
export type Kind = 'agent' | 'web' | 'service' | 'gui' | 'test' | 'shell'

// SERVER_KIND_MAP folds the manifest's eight kinds onto the six UI bodies;
// code/app/verdict render as 'web' until they get their own surfaces.
const SERVER_KIND_MAP: Record<string, Kind> = {
  shell: 'shell',
  code: 'web',
  app: 'web',
  agent: 'agent',
  screen: 'gui',
  test: 'test',
  verdict: 'web',
  service: 'service',
}

// agentFor returns the template's agent metadata, preferring the server manifest over AGENT_CONFIG.
export function agentFor(t: Template): AgentConfig | null {
  const a = t.agent
  if (a) {
    return {
      keyEnv: a.key_env ?? '',
      keyLabel: a.key_label ?? 'API key',
      keyNote: a.key_note,
      runCmd: a.run_cmd ?? '',
      taskPlaceholder: a.task_placeholder ?? '',
      baseUrlEnv: a.base_url_env ?? '',
      modelEnv: a.model_env ?? '',
      mcpFile: a.config_file ?? '',
      configLabel: a.config_label ?? 'Config',
      configFormat: a.config_format === 'toml' ? 'toml' : 'json',
      configExample: a.config_example ?? '',
      installCmd: a.install_cmd ?? '',
      cliBin: a.cli_bin ?? '',
      runFlags: a.run_flags ?? '',
      docsUrl: t.docs_url ?? AGENT_CONFIG[t.id]?.docsUrl ?? '',
    }
  }
  return AGENT_CONFIG[t.id] ?? null
}

// kindOf returns the manifest kind, falling back to field heuristics for manifest-less templates.
export function kindOf(t: Template): Kind {
  if (t.kind && SERVER_KIND_MAP[t.kind]) return SERVER_KIND_MAP[t.kind]
  const tags = (t.tags ?? []).map(x => x.toLowerCase())
  const has = (...needles: string[]) => needles.some(n => tags.includes(n))
  const lang = (t.language ?? '').toLowerCase()

  // Not the generic `agent` tag: OpenClaw is a base for building agents, not a coding CLI.
  if (AGENT_CONFIG[t.id] || has('coding-cli')) return 'agent'

  if (
    t.has_server &&
    (lang === 'android' || t.id === 'desktop' || has('vnc', 'emulator', 'computer-use'))
  ) {
    return 'gui'
  }

  if (t.image || t.category === 'database' || t.category === 'queue') return 'service'

  if (has('testing', 'e2e', 'playwright')) return 'test'

  if (t.id === 'base') return 'shell'

  return 'web'
}

// AccessMode is how you reach the running agent, which decides what the preview URL serves.
export type AccessMode = 'terminal' | 'task'

export interface AccessModeMeta {
  label: string
  help: string
  access: string
  previewLabel: string
  cta: string
  showTask: boolean
}

export const ACCESS_MODES: Record<AccessMode, AccessModeMeta> = {
  terminal: {
    label: 'Web terminal',
    help: 'The preview URL opens a live terminal (ttyd) running the agent — drive it from the browser. Works for any CLI agent.',
    access: 'Web terminal',
    previewLabel: 'Terminal URL',
    cta: '▶ Run agent',
    showTask: true,
  },
  task: {
    label: 'Run task & serve result',
    help: 'Runs the task once, then serves the agent workspace on the preview URL so you get one finished link to see what it built.',
    access: 'Run & serve',
    previewLabel: 'Preview when done',
    cta: '▶ Run agent',
    showTask: true,
  },
}

// b64utf8 base64-encodes UTF-8 text; btoa alone throws on non-Latin1 code points.
export function b64utf8(s: string): string {
  const bytes = encodeURIComponent(s).replace(/%([0-9A-F]{2})/g, (_, h) =>
    String.fromCharCode(parseInt(h, 16)),
  )
  if (typeof btoa === 'function') return btoa(bytes)
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return (globalThis as any).Buffer.from(bytes, 'binary').toString('base64')
}

export function ctaLabel(kind: Kind, mode: AccessMode): string {
  switch (kind) {
    case 'agent':
      return ACCESS_MODES[mode].cta
    case 'web':
      return '↑ Deploy sandbox'
    case 'service':
      return '↑ Start service'
    case 'gui':
      return '↑ Launch'
    case 'test':
      return '▶ Run tests'
    case 'shell':
      return '↑ Create sandbox'
  }
}

export function previewLabel(kind: Kind, mode: AccessMode): string {
  if (kind === 'agent') return ACCESS_MODES[mode].previewLabel
  return 'Preview URL'
}

// ComposedBody is the kind-specific slice of a CreateSandboxReq; the caller
// merges it with the shared fields and any page-specific files.
export interface ComposedBody {
  env?: string[]
  /** Vault secret names the server resolves to env vars at create. */
  secret_refs?: string[]
  /** Agent launch args; the portal expands them from the template manifest so dashboard, CLI and SDK share one path. */
  args?: { task?: string; access_mode?: string }
  /** Autonomy posture. A request field, not an env row, so the server can refuse a value it cannot honour. */
  agent_mode?: 'supervised' | 'guided' | 'autonomous'
  files?: Array<{ path: string; content: string }>
  start?: string
  port?: number
  // Runs before start; undefined means use the caller's own setup lines.
  setup?: string[]
  // undefined means the backend default applies.
  timeout?: number
  idle_timeout?: number
}

// Keep-alive defaults for deploys that serve a preview, used when Advanced is blank.
export const KEEPALIVE_IDLE = -1
export const KEEPALIVE_TIMEOUT = 3600

// startsServer reports whether a deploy of this kind serves something on the preview URL.
export function startsServer(kind: Kind, start: string): boolean {
  switch (kind) {
    case 'agent':
    case 'gui':
    case 'service':
    case 'test':
      return true
    case 'web':
    case 'shell':
      return start.trim() !== ''
  }
}

// resolveLifetime returns the effective timeout and idle_timeout. A typed value
// always wins; blank falls back to keep-alive for server deploys, else undefined.
export function resolveLifetime(
  serves: boolean,
  timeoutInput: string,
  idleInput: string,
): { timeout?: number; idle_timeout?: number } {
  const resolve = (input: string, keepAlive: number): number | undefined => {
    if (input.trim() !== '') return parseInt(input, 10)
    return serves ? keepAlive : undefined
  }
  return {
    timeout: resolve(timeoutInput, KEEPALIVE_TIMEOUT),
    idle_timeout: resolve(idleInput, KEEPALIVE_IDLE),
  }
}

export const CATEGORY_ORDER = [
  'Agents',
  'Use cases',
  'Databases',
  'Python',
  'Node / Web',
  'Systems',
  'Browser / E2E',
  'Mobile',
  'Other',
] as const

export type Category = (typeof CATEGORY_ORDER)[number]

export function categoryOf(t: Template): Category {
  const hay = [t.id, t.language ?? '', ...(t.tags ?? [])].join(' ').toLowerCase()
  const has = (...needles: string[]) => needles.some(n => hay.includes(n))
  // Whole tokens only: substring matching filed "mongodb" under Systems via "go".
  const hasWord = (...needles: string[]) => {
    const words = hay.split(/[\s\-_/.]+/)
    return needles.some(n => words.includes(n))
  }

  // Order matters: use cases and agents would otherwise match their stack.
  if (has('use-case')) return 'Use cases'
  if (has('claude', 'codex', 'grok', 'opencode', 'amp', 'agent')) return 'Agents'
  if (hasWord('database', 'queue', 'postgres', 'postgresql', 'redis', 'mysql', 'mongodb', 'mongo', 'kafka', 'sql', 'nosql'))
    return 'Databases'
  if (has('android', 'mobile', 'appium', 'ios', 'emulator')) return 'Mobile'
  if (has('playwright', 'puppeteer', 'selenium', 'browser', 'e2e', 'openclaw', 'desktop', 'vnc'))
    return 'Browser / E2E'
  if (hasWord('go', 'golang', 'rust', 'cargo', 'c++', 'cpp', 'systems')) return 'Systems'
  if (hasWord('node', 'typescript', 'javascript', 'next', 'nextjs', 'ts', 'js', 'web', 'deno', 'bun'))
    return 'Node / Web'
  if (has('python', 'fastapi', 'flask', 'django', 'code-interpreter', 'jupyter'))
    return 'Python'
  return 'Other'
}

export const sizeLabel = (mb: number) => (mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`)

// withCur keeps the current value selectable even when it is not in the base list.
export const withCur = (base: number[], cur: number) =>
  Array.from(new Set([...base, cur])).sort((a, b) => a - b)

// shellQuote single-quotes a value when a shell would otherwise interpret it.
export function shellQuote(v: string): string {
  if (v === '') return "''"
  if (/^[A-Za-z0-9_/.:=@-]+$/.test(v)) return v
  return `'${v.replace(/'/g, `'\\''`)}'`
}

// The UI never offers a runtime picker; only container-required templates pin docker.

function isAndroidTemplate(t: Template): boolean {
  if ((t.language ?? '').toLowerCase() === 'android') return true
  if (t.id.startsWith('android')) return true
  if (t.id === 'mobile-test') return true
  const tags = (t.tags ?? []).map(x => x.toLowerCase())
  return tags.some(x => x === 'emulator' || x === 'appium' || x === 'android')
}

function isImageBackedService(t: Template): boolean {
  if (t.image) return true
  const cat = (t.category ?? '').toLowerCase()
  return cat === 'database' || cat === 'queue'
}

// requiredRuntime returns the runtime to pin on create, or undefined for auto.
export function requiredRuntime(t: Template): string | undefined {
  if (isAndroidTemplate(t) || isImageBackedService(t)) return 'docker'
  return undefined
}

// runsOnLabel is the read-only "Runs on" text.
export function runsOnLabel(t: Template): string {
  if (isAndroidTemplate(t)) return 'Docker · KVM'
  if (isImageBackedService(t)) return 'Docker'
  return 'Auto'
}

// ServiceConn is what the Connection block shows for a database or queue
// template, whose endpoint speaks a protocol rather than HTTP.
export interface ServiceConn {
  /** Best-effort host string. */
  host: string
  port: number
  credEnvNames: string[]
  /** Set when the scheme is known and the credentials are available. */
  url?: string
}

// serviceUrlFor returns undefined when the scheme needs a credential that is missing.
function serviceUrlFor(
  t: Template,
  host: string,
  port: number,
  env: Record<string, string>,
): string | undefined {
  switch (t.id) {
    case 'postgres': {
      const pass = env['POSTGRES_PASSWORD'] ?? ''
      const db = env['POSTGRES_DB'] || 'app'
      return pass ? `postgres://postgres:${pass}@${host}:${port}/${db}` : undefined
    }
    case 'mysql': {
      const pass = env['MYSQL_ROOT_PASSWORD'] ?? ''
      return pass ? `mysql://root:${pass}@${host}:${port}/` : undefined
    }
    case 'redis':
      return `redis://${host}:${port}`
    case 'mongodb':
      return `mongodb://${host}:${port}`
    default:
      return undefined
  }
}

function serviceHostForId(id?: string): string {
  return sandboxHost(id ?? '<id>')
}

// serviceConn derives the Connection block info for a service template.
export function serviceConn(t: Template, id?: string, env?: Record<string, string>): ServiceConn {
  const port = t.default_port ?? t.port ?? t.ports?.[0] ?? 5432
  const credEnvNames = (t.env ?? []).map(e => e.name)
  const host = serviceHostForId(id)
  return { host, port, credEnvNames, url: serviceUrlFor(t, host, port, env ?? {}) }
}

export interface CustomEnv {
  name: string
  value: string
}

// buildEnvList assembles NAME=VALUE rows; blank names and empty declared values are dropped.
export function buildEnvList(
  declared: TemplateEnv[],
  values: Record<string, string>,
  custom: CustomEnv[],
): string[] {
  const out: string[] = []
  for (const e of declared) {
    const v = values[e.name] ?? ''
    if (v !== '') out.push(`${e.name}=${v}`)
  }
  for (const row of custom) {
    const name = row.name.trim()
    if (name) out.push(`${name}=${row.value}`)
  }
  return out
}

/** envPlaceholder shows the declared default, else whether the field is required. */
export function envPlaceholder(e: { value?: string; required?: boolean }): string {
  if (e.value) return e.value
  return e.required ? 'required' : 'optional'
}

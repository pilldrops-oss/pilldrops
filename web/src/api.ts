import type {
  CreateSandboxReq,
  CreateSandboxResp,
  ExecResp,
  StatusResp,
  ApiKey,
  CreateKeyResp,
  UsageResp,
  HostResp,
  NodeInfo,
  Template,
  FilesystemListResp,
  SandboxDetail,
  SandboxLogsResp,
  SandboxConfig,
  McpServer,
  CronJob,
  ChatMessage,
  ChannelInfo,
  PreviewStatus,
  FunctionInfo,
  RegisterFunctionReq,
  UpdateFunctionReq,
  InvokeResult,
  FunctionMetrics,
  FunctionCode,
  InvokeOptions,
  LimitsInfo,
  VaultSecret,
  CustomDomain,
  ClusterStats,
  JoinToken,
  ImageInfo,
  PatchImageReq,
  SandboxRow,
  HistoryRecord,
  ConcPoint,
  EventRow,
  AdminUser,
  AdminUserDetail,
  AdminOverview,
  AdminSessions,
  AdminErrorRow,
  Project,
  ProjectRun,
  RunService,
  AdminSandboxDetail,
  FeedbackRow,
  WaitlistRow,
  SandboxMetrics,
  SandboxPorts,
  DeployProgress,
} from './types'

function getKey(): string {
  return window.__SANDBOXD__?.key ?? ''
}

// The API key is the only credential. SSE calls rely on the session cookie instead,
// since EventSource cannot set headers.
async function bearerToken(): Promise<string> {
  return getKey()
}

// No key (make dev's anonymous server) sends no Authorization header at all.
async function authHeaders(): Promise<HeadersInit> {
  const key = await bearerToken()
  return key
    ? { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' }
    : { 'Content-Type': 'application/json' }
}

// ApiError carries a UI-safe message plus the status, server code, and method/path to branch on.
export class ApiError extends Error {
  status: number
  code?: string
  method: string
  path: string
  constructor(opts: { status: number; code?: string; method: string; path: string; message: string }) {
    super(opts.message)
    this.name = 'ApiError'
    this.status = opts.status
    this.code = opts.code
    this.method = opts.method
    this.path = opts.path
  }
}

// statusFallback is used when the response body has no usable error field.
function statusFallback(status: number): string {
  switch (status) {
    case 400: return 'Something in the request wasn’t valid — check your inputs and try again.'
    case 401: return 'You’re signed out or your session expired. Sign in again and retry.'
    case 403: return 'You don’t have permission to do that.'
    case 404: return 'Not found — it may have been paused, removed, or never existed.'
    case 408: return 'The request timed out. Please try again.'
    case 409: return 'That conflicts with the current state — refresh and try again.'
    case 413: return 'That upload is too large.'
    case 429: return 'You’ve hit a plan or rate limit — pause a sandbox or upgrade, then retry.'
    case 500: return 'The server hit an unexpected error. Please try again.'
    case 502:
    case 503:
    case 504: return 'The service is temporarily unavailable. Please try again in a moment.'
    default: return status >= 500
      ? 'The server hit an unexpected error. Please try again.'
      : `Request failed (${status}).`
  }
}

// friendlyError extracts a UI-safe message from a failed response body; never a raw JSON blob.
function friendlyError(status: number, raw: string): { message: string; code?: string } {
  let code: string | undefined
  let message = ''
  const trimmed = raw.trim()
  if (trimmed) {
    try {
      const j = JSON.parse(trimmed) as { error?: string; message?: string; detail?: string; code?: string }
      message = (j.error || j.message || j.detail || '').trim()
      code = j.code
    } catch {
      // Non-JSON: use short plain-text bodies verbatim, but ignore HTML pages.
      if (trimmed.length < 200 && !trimmed.startsWith('<')) message = trimmed
    }
  }
  if (!message) message = statusFallback(status)
  return { message, code }
}

// The login-bounce marker lives for this tab and 30 seconds: long enough to
// catch an immediate redirect back, short enough that a later expiry still
// sends the user to sign in. Storage can be unavailable; then it never blocks.
const LOGIN_BOUNCE = 'pd-login-bounce'
function loginBounced(): boolean {
  try {
    const t = Number(sessionStorage.getItem(LOGIN_BOUNCE))
    return t > 0 && Date.now() - t < 30_000
  } catch { return false }
}
function markLoginBounce() {
  try { sessionStorage.setItem(LOGIN_BOUNCE, String(Date.now())) } catch { /* no storage */ }
}
function clearLoginBounce() {
  try { sessionStorage.removeItem(LOGIN_BOUNCE) } catch { /* no storage */ }
}

async function request<T>(method: string, path: string, body?: unknown, auth = true): Promise<T> {
  const headers: HeadersInit = auth ? await authHeaders() : { 'Content-Type': 'application/json' }
  const res = await fetch(path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    // A 401 means the session is missing or stale: go and sign in. Once only —
    // a server whose login page sends the browser straight back would loop.
    if (
      res.status === 401 &&
      !window.location.pathname.endsWith('/dashboard/login') &&
      !loginBounced()
    ) {
      markLoginBounce()
      window.location.href = '/dashboard/login'
      throw new Error('unauthenticated — redirecting to login')
    }
    const raw = await res.text().catch(() => '')
    const { message, code } = friendlyError(res.status, raw)
    throw new ApiError({ status: res.status, code, method, path, message })
  }
  clearLoginBounce()
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

// checkAdminAccess bypasses request() so a 403 (not whitelisted) is not redirected like a 401.
export async function checkAdminAccess(): Promise<'ok' | 'forbidden' | 'unauth'> {
  try {
    const res = await fetch('/v1/admin/whoami', { headers: await authHeaders() })
    if (res.ok) return 'ok'
    if (res.status === 403) return 'forbidden'
    return 'unauth'
  } catch {
    return 'unauth'
  }
}

export const createSandbox = (body: CreateSandboxReq) =>
  // client:'web' attributes the create to the web UI.
  request<CreateSandboxResp>('POST', '/v1/sandboxes', { ...body, client: 'web' })

export const execInSandbox = (id: string, cmd: string, cwd?: string, timeout?: number) =>
  request<ExecResp>('POST', `/v1/sandboxes/${id}/exec`, { cmd, cwd, timeout })

export const pauseSandbox = (id: string) =>
  request<StatusResp>('POST', `/v1/sandboxes/${id}/pause`, {})

// ChatLinkResp is a single-use, short-lived code that binds a chat account to the workstation.
export type ChatLinkResp = {
  code: string
  read_only: boolean
  autonomous: boolean
  telegram_url: string
  expires_in: string
}

export const createChatLink = (id: string, autonomous: boolean) =>
  request<ChatLinkResp>('POST', `/v1/sandboxes/${id}/chat-link`, { autonomous })

export const revokeChatLinks = (id: string) =>
  request<{ status: string }>('DELETE', `/v1/sandboxes/${id}/chat-link`)

export type ForkResp = CreateSandboxResp & {
  parent_id: string
  parent_state: string
  boot_ms: number
  parent_paused_ms: number
}

export const forkSandbox = (id: string, name?: string) =>
  request<ForkResp>('POST', `/v1/sandboxes/${id}/fork`, name ? { name } : {})

export const resumeSandbox = (id: string) =>
  request<StatusResp>('POST', `/v1/sandboxes/${id}/resume`, {})

// VisibilityResp says what the flip did. `wiped` lists the agent logins a
// share removed from the sandbox; `relogin` marks an unshare, which restores
// nothing — there is nowhere to restore from.
export type VisibilityResp = {
  id: string
  visibility: 'org' | 'private'
  was: 'org' | 'private'
  wiped?: string[]
  wipe_skipped?: string
  relogin?: boolean
}

export const setSandboxVisibility = (id: string, visibility: 'org' | 'private') =>
  request<VisibilityResp>('POST', `/v1/sandboxes/${id}/visibility`, { visibility })

export const refreshSandbox = (id: string) =>
  request<StatusResp>('POST', `/v1/sandboxes/${id}/refresh`, {})

export const setSandboxTimeout = (id: string, seconds: number) =>
  request<void>('POST', `/v1/sandboxes/${id}/timeout`, { seconds })

export const destroySandbox = (id: string) =>
  request<void>('DELETE', `/v1/sandboxes/${id}`, undefined)

export const getSandbox = (id: string) =>
  request<SandboxDetail>('GET', `/v1/sandboxes/${id}`, undefined)

// renameSandbox sets or ('') clears the label; the response carries the server-sanitized value.
export const renameSandbox = (id: string, name: string) =>
  request<SandboxDetail>('PATCH', `/v1/sandboxes/${id}`, { name })

// setSandboxSlug claims or ('') releases the slug; 409 when another sandbox holds it.
export const setSandboxSlug = (id: string, slug: string) =>
  request<SandboxDetail>('PATCH', `/v1/sandboxes/${id}`, { slug })

// A Go nil slice serialises as null; normalise to [] so no consumer trips on it.
export const listSandboxes = () =>
  request<{ sandboxes: SandboxRow[] | null }>('GET', '/v1/sandboxes', undefined).then(r => ({
    sandboxes: r.sandboxes ?? [],
  }))

// getDeployStatus reports async_setup progress, distinct from the sandbox's own logs.
export const getDeployStatus = (id: string) =>
  request<DeployProgress>('GET', `/v1/sandboxes/${id}/deploy`, undefined)

export const getSandboxLogs = (id: string, limit = 500) =>
  request<SandboxLogsResp>('GET', `/v1/sandboxes/${id}/logs?limit=${limit}`, undefined)

export const getPreviewStatus = (id: string, port?: number) =>
  request<PreviewStatus>(
    'GET',
    `/v1/sandboxes/${encodeURIComponent(id)}/preview-status${port ? `?port=${port}` : ''}`,
  )

export const getSandboxMetrics = (id: string) =>
  request<SandboxMetrics>('GET', `/v1/sandboxes/${encodeURIComponent(id)}/metrics`, undefined)

export const getSandboxPorts = (id: string) =>
  request<SandboxPorts>('GET', `/v1/sandboxes/${encodeURIComponent(id)}/ports`, undefined)

export const fsList = (id: string, path: string) =>
  request<FilesystemListResp>('GET', `/v1/sandboxes/${id}/filesystem/list?path=${encodeURIComponent(path)}`, undefined)

export const fsRead = async (id: string, path: string): Promise<string> => {
  const key = await bearerToken()
  return fetch(`/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, {
    headers: { Authorization: `Bearer ${key}` },
  }).then(r => {
    if (!r.ok) throw new Error(`GET /v1/sandboxes/${id}/files → ${r.status}`)
    return r.text()
  })
}

// content may be a Blob (a File from a drop or a picker), which is sent as-is
// rather than read into a string first: /files streams the body to the sandbox
// under its own PD_FILES_MAX_BYTES, so a file far larger than memory-as-text
// can go through.
export const fsWrite = async (id: string, path: string, content: string | Blob): Promise<void> => {
  const key = await bearerToken()
  const r = await fetch(`/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${key}`,
      'Content-Type': typeof content === 'string' ? 'text/plain' : 'application/octet-stream',
    },
    body: content,
  })
  if (!r.ok) {
    // The server's own message is what says *why* — a 413 naming the file
    // limit reads very differently from a bare status.
    const body = await r.text().catch(() => '')
    throw new Error(`POST /v1/sandboxes/${id}/files → ${r.status}: ${body}`)
  }
}

// listHistory returns past sandboxes newest-first without logs; getHistory includes them.
export const listHistory = (limit = 200, offset = 0) =>
  request<{ history: HistoryRecord[] }>('GET', `/v1/history?limit=${limit}&offset=${offset}`, undefined)

export const getHistory = (id: string) =>
  request<HistoryRecord>('GET', `/v1/history/${id}`, undefined)

export const listKeys = () => request<ApiKey[]>('GET', '/v1/keys', undefined)
export const createKey = (name: string) => request<CreateKeyResp>('POST', '/v1/keys', { name })
export const deleteKey = (id: string) => request<void>('DELETE', `/v1/keys/${id}`, undefined)

export const getUsage = () => request<UsageResp>('GET', '/v1/usage', undefined)

export const getConcurrency = (range: string) =>
  request<{ range: string; points: ConcPoint[] }>('GET', `/v1/concurrency?range=${range}`, undefined)
export const getEvents = (limit = 50) =>
  request<{ events: EventRow[] }>('GET', `/v1/events?limit=${limit}`, undefined)

export const getHost = () => request<HostResp>('GET', '/v1/host', undefined)

export const listNodes = () => request<{ nodes: NodeInfo[] }>('GET', '/v1/nodes', undefined)

export const getClusterStats = () => request<ClusterStats>('GET', '/v1/cluster/stats', undefined)

// mintJoinToken returns a one-time join token plus the one-line node bootstrap command.
export const mintJoinToken = () => request<JoinToken>('POST', '/v1/join-tokens', {})

export const cordonNode = (id: string) =>
  request<{ ok: boolean }>('POST', `/v1/nodes/${id}/cordon`, {})

export const uncordonNode = (id: string) =>
  request<{ ok: boolean }>('POST', `/v1/nodes/${id}/uncordon`, {})

export const drainNode = (id: string) =>
  request<{ ok: boolean }>('POST', `/v1/nodes/${id}/drain`, {})

export const removeNode = (id: string) =>
  request<{ ok: boolean }>('DELETE', `/v1/nodes/${id}`, undefined)

export const listImages = () => request<{ images: ImageInfo[] }>('GET', '/v1/images', undefined)

export const patchImage = (id: string, body: PatchImageReq) =>
  request<ImageInfo>('PATCH', `/v1/images/${id}`, body)

export const listTemplates = () => request<Template[]>('GET', '/v1/templates', undefined)

// listPublicTemplates is the same catalog unauthenticated, the signed-out fallback.
export const listPublicTemplates = () =>
  request<Template[]>('GET', '/v1/templates/public', undefined, false)

// RevisedPlanResp is the server-validated reply from the BYOK plan reviser.
export interface RevisedPlanResp {
  services: Array<{ template: string; reason: string }>
  note: string
}


// ThreadRow is a persisted conversation; `turns` is an opaque transcript the Build page owns.
export interface ThreadRow {
  id: string
  title: string
  /** Present on get/create/patch; omitted from the list on purpose. */
  turns?: string
  created_at: number
  updated_at: number
}

export const listThreads = () =>
  request<{ threads: ThreadRow[] | null }>('GET', '/v1/threads', undefined)

export const createThread = (title: string, turns: string) =>
  request<ThreadRow>('POST', '/v1/threads', { title, turns })

export const getThread = (id: string) =>
  request<ThreadRow>('GET', `/v1/threads/${id}`, undefined)

export const patchThread = (id: string, body: { title?: string; turns?: string }) =>
  request<ThreadRow>('PATCH', `/v1/threads/${id}`, body)

export const deleteThread = (id: string) =>
  request<void>('DELETE', `/v1/threads/${id}`, undefined)

// p50_ms is 0 when the server withheld it (too few samples); check n before showing an ETA.
export const getLaunchStats = (templates: string[]) =>
  request<{ stats: Record<string, { p50_ms: number; n: number }> }>(
    'GET',
    `/v1/launch-stats?templates=${encodeURIComponent(templates.join(','))}`,
    undefined,
  )

// postLaunchSample reports one verified launch's create-to-serving duration. Only report
// launches that actually served.
export const postLaunchSample = (template: string, ms: number) =>
  request<void>('POST', '/v1/launch-stats', { template, ms })

export const listFunctions = () =>
  request<{ functions: FunctionInfo[] }>('GET', '/v1/functions', undefined)

export const getFunction = (name: string) =>
  request<FunctionInfo>('GET', `/v1/functions/${name}`, undefined)

export const registerFunction = (body: RegisterFunctionReq) =>
  request<FunctionInfo>('POST', '/v1/functions', body)

export const deleteFunction = (name: string) =>
  request<void>('DELETE', `/v1/functions/${name}`, undefined)

export const getFunctionLogs = (name: string) =>
  request<SandboxLogsResp>('GET', `/v1/functions/${name}/logs`, undefined)

export const updateFunction = (name: string, body: UpdateFunctionReq) =>
  request<StatusResp>('PUT', `/v1/functions/${name}`, body)

export const getFunctionCode = (name: string) =>
  request<FunctionCode>('GET', `/v1/functions/${name}/code`, undefined)

export const getFunctionMetrics = (name: string) =>
  request<FunctionMetrics>('GET', `/v1/functions/${name}/metrics`, undefined)

export async function invokeFunction(name: string, opts: InvokeOptions = {}): Promise<InvokeResult> {
  const method = opts.method ?? 'POST'
  let path = opts.path ?? ''
  if (path && !path.startsWith('/')) path = `/${path}`
  const headers: Record<string, string> = {
    Authorization: `Bearer ${await bearerToken()}`,
    ...(opts.headers ?? {}),
  }
  const res = await fetch(`/v1/functions/${name}/invoke${path}`, {
    method,
    headers,
    body: method === 'GET' || method === 'HEAD' ? undefined : (opts.body ?? ''),
  })
  const text = await res.text()
  const respHeaders: Record<string, string> = {}
  res.headers.forEach((v, k) => { respHeaders[k] = v })
  return {
    status: res.status,
    body: text,
    cold_start: res.headers.get('X-Sandboxd-Cold-Start') === 'true',
    duration_ms: Number(res.headers.get('X-Sandboxd-Duration-Ms') ?? 0),
    headers: respHeaders,
  }
}

export const listSecrets = () => request<VaultSecret[]>('GET', '/v1/secrets', undefined)
export const putSecret = (name: string, value: string) =>
  request<VaultSecret>('POST', '/v1/secrets', { name, value })
export const deleteSecret = (name: string) =>
  request<void>('DELETE', `/v1/secrets/${encodeURIComponent(name)}`, undefined)

export const listDomains = async () =>
  (await request<{ domains: CustomDomain[] }>('GET', '/v1/domains', undefined)).domains
export const addDomain = (host: string, sandboxId?: string, port?: number) =>
  request<CustomDomain>('POST', '/v1/domains', { host, sandbox_id: sandboxId || undefined, port })
export const removeDomain = (host: string) =>
  request<void>('DELETE', `/v1/domains/${encodeURIComponent(host)}`, undefined)

export const getLimits = () =>
  request<LimitsInfo>('GET', '/v1/limits', undefined)

// joinWaitlist hits the portal's public form endpoint, not /v1, so no auth.
export const joinWaitlist = (email: string, source: string, website = '') =>
  request<{ ok: boolean }>('POST', '/waitlist', { email, source, website }, false)

export async function listAdminUsers(limit = 200): Promise<{ users: AdminUser[]; count: number }> {
  return request('GET', `/v1/admin/users?limit=${limit}`)
}

// getPreviewToken mints a port-bound preview token on demand. Needs the API credential;
// holding one preview token is deliberately not enough to mint another.
export async function getPreviewToken(id: string, port: number | string): Promise<string | null> {
  const r = await request<{ token?: string }>(
    'GET',
    `/v1/sandboxes/${encodeURIComponent(id)}/preview-token?port=${encodeURIComponent(String(port))}`,
  )
  return r.token ?? null
}

export async function getAdminUser(ownerId: string): Promise<AdminUserDetail> {
  return request('GET', `/v1/admin/users/${encodeURIComponent(ownerId)}`)
}

export async function getAdminOverview(days = 14): Promise<AdminOverview> {
  return request('GET', `/v1/admin/overview?days=${days}`)
}

export async function getAdminSessions(days = 30): Promise<AdminSessions> {
  return request('GET', `/v1/admin/sessions?days=${days}`)
}

export async function getAdminErrors(limit = 200): Promise<{ errors: AdminErrorRow[]; count: number }> {
  return request('GET', `/v1/admin/errors?limit=${limit}`)
}

export async function getAdminFeedback(limit = 300): Promise<{ feedback: FeedbackRow[]; count: number }> {
  return request('GET', `/v1/admin/feedback?limit=${limit}`)
}

export async function getAdminWaitlist(limit = 300): Promise<{ waitlist: WaitlistRow[]; count: number }> {
  return request('GET', `/v1/admin/waitlist?limit=${limit}`)
}

export async function getAdminHistory(id: string): Promise<HistoryRecord> {
  return request('GET', `/v1/admin/history/${encodeURIComponent(id)}`)
}

export const adminSandbox = (id: string) =>
  request<AdminSandboxDetail>('GET', `/v1/admin/sandboxes/${encodeURIComponent(id)}`)

// previewUrl and isPreviewCrossOrigin both derive from this so they cannot disagree.
// index.html ships the literal __SANDBOXD_DOMAIN__; an unsubstituted placeholder means unset.
function previewDomain(): string | undefined {
  const d = window.__SANDBOXD__?.sandboxDomain
  return d && !d.startsWith('__') ? d : undefined
}

export function previewUrl(id: string, port: number | string): string {
  const d = previewDomain()
  if (d) return `https://${port}-${id}.${d}/`
  return `/preview/${id}/${port}/`
}

// serverOrigin is the PillDrops server this dashboard was served by, for
// snippets pasted into a terminal or another tool.
export function serverOrigin(): string {
  return window.location.origin
}

// absolutePreviewUrl is previewUrl as a full URL, for the same snippets.
export function absolutePreviewUrl(id: string, port: number | string): string {
  const u = previewUrl(id, port)
  return u.startsWith('/') ? serverOrigin() + u : u
}

// sandboxHost is the hostname a sandbox's services answer on: its label under
// the preview domain, or this server's host when there is none.
export function sandboxHost(label: string): string {
  return `${label}.${previewDomain() ?? window.location.hostname}`
}

// slugUrl is the sandbox's named host, https://<slug>.<domain>/; undefined
// without a slug or a preview domain (the path form has no name to hang it on).
export function slugUrl(slug: string | undefined): string | undefined {
  const d = previewDomain()
  if (!slug || !d) return undefined
  return `https://${slug}.${d}/`
}

// isPreviewCrossOrigin reports whether previews are safe to iframe with allow-same-origin.
// Same-origin previews would let sandbox code reach window.parent, so callers must not
// embed then. Fails closed when no domain is configured.
export function isPreviewCrossOrigin(): boolean {
  return previewDomain() !== undefined
}

// The portal mounts project routes only when it has a store, so a 404 here means
// "not available on this deployment", not broken. Callers degrade rather than error.

export const listProjects = () =>
  request<{ projects: Project[] }>('GET', '/v1/projects')

export const createProject = (name: string, plan?: string) =>
  request<Project>('POST', '/v1/projects', { name, plan })

export const getProject = (id: string) =>
  request<Project>('GET', `/v1/projects/${encodeURIComponent(id)}`)

export const updateProject = (id: string, patch: { name?: string; plan?: string }) =>
  request<Project>('PATCH', `/v1/projects/${encodeURIComponent(id)}`, patch)

export const archiveProject = (id: string) =>
  request<void>('DELETE', `/v1/projects/${encodeURIComponent(id)}`)

export const listRuns = (id: string) =>
  request<{ runs: ProjectRun[] }>('GET', `/v1/projects/${encodeURIComponent(id)}/runs`)

export const recordRun = (id: string, body: { plan_snapshot?: string; services?: RunService[] }) =>
  request<ProjectRun>('POST', `/v1/projects/${encodeURIComponent(id)}/runs`, body)

export const endRun = (projectID: string, runID: string) =>
  request<void>('PATCH', `/v1/projects/${encodeURIComponent(projectID)}/runs/${encodeURIComponent(runID)}`)

// Config values apply to the next command without restarting the machine.
export async function getSandboxConfig(id: string): Promise<SandboxConfig> {
  return request('GET', `/v1/sandboxes/${encodeURIComponent(id)}/config`)
}

/** Sends only what changed; the server merges, and an empty string removes a variable. */
export async function setSandboxConfig(
  id: string,
  patch: { env?: Record<string, string>; model?: string; base_url?: string },
): Promise<SandboxConfig> {
  return request('POST', `/v1/sandboxes/${encodeURIComponent(id)}/config`, patch)
}

export async function listIntegrations(): Promise<{ servers: McpServer[]; agents: string[] }> {
  return request('GET', '/v1/integrations')
}

export async function getSandboxIntegrations(id: string): Promise<{ servers: string[] }> {
  return request('GET', `/v1/sandboxes/${encodeURIComponent(id)}/integrations`)
}

/** The whole selection is sent every time: "off" has to mean gone. */
export async function setSandboxIntegrations(
  id: string,
  servers: string[],
  env: Record<string, string>,
): Promise<{ servers: string[]; path: string; note: string }> {
  return request('POST', `/v1/sandboxes/${encodeURIComponent(id)}/integrations`, { servers, env })
}

export async function listCronJobs(): Promise<{ jobs: CronJob[] }> {
  return request('GET', '/v1/cron')
}

export async function createCronJob(job: {
  sandbox_id: string
  schedule: string
  command: string
  name?: string
  timezone?: string
}): Promise<CronJob> {
  return request('POST', '/v1/cron', job)
}

export async function runCronJobNow(id: string): Promise<{ status: string; exit_code: number; output: string }> {
  return request('POST', `/v1/cron/${encodeURIComponent(id)}/run`)
}

export async function toggleCronJob(id: string, enabled: boolean): Promise<unknown> {
  return request('POST', `/v1/cron/${encodeURIComponent(id)}/toggle`, { enabled })
}

export async function deleteCronJob(id: string): Promise<unknown> {
  return request('DELETE', `/v1/cron/${encodeURIComponent(id)}`)
}

export async function getChatHistory(id: string): Promise<{ messages: ChatMessage[] }> {
  return request('GET', `/v1/sandboxes/${encodeURIComponent(id)}/chat`)
}

export async function sendChat(id: string, message: string, autonomous = false): Promise<{ reply: string }> {
  return request('POST', `/v1/sandboxes/${encodeURIComponent(id)}/chat`, { message, autonomous })
}

export async function getChannels(id: string): Promise<{ channels: ChannelInfo[]; bot_username: string }> {
  return request('GET', `/v1/sandboxes/${encodeURIComponent(id)}/channels`)
}

/** Mints a single-use pairing code; read_only yields a link that can report but never run anything. */
export async function createPairingCode(
  id: string,
  opts: { read_only?: boolean; autonomous?: boolean } = {},
): Promise<{ code: string; telegram_url: string; expires_in: string; read_only: boolean; autonomous: boolean }> {
  return request('POST', `/v1/sandboxes/${encodeURIComponent(id)}/chat-link`, opts)
}

// Agent routes exist only for kind=agent templates; the server 404s otherwise.

export type AgentTurn = {
  id: string
  request: string
  state: 'queued' | 'running' | 'awaiting_approval' | 'done' | 'failed' | 'interrupted'
  mode?: string
  follow_up?: boolean
  source?: string
  priority?: boolean
  queued_at: string
  started_at?: string
  ended_at?: string
  exit_code?: number
  error?: string
  response?: string
  session_id?: string
  seq_start?: number
  seq_end?: number
  /** Absent means unknown, not zero; only some agents report usage. */
  cost_usd?: number
}

export type AgentSpend = {
  cap_usd: number
  spent_usd: number
  remaining_usd?: number
  exceeded: boolean
  /** Turns whose agent reported no cost. */
  turns_unreported: number
  coverage: string
}

/** AgentAutonomy is server-reported so the supported-modes list cannot drift from the agent. */
export type AgentAutonomy = {
  /** Selectable modes, narrowest first. */
  available: string[]
  default: string
  guided_unavailable_reason?: string
}

export type AgentSession = {
  state: 'starting' | 'idle' | 'busy' | 'degraded' | 'stopped'
  /** rich = a tool timeline is available; basic = text only. */
  mode: 'rich' | 'basic'
  degraded_reason?: string
  current_turn?: AgentTurn
  queue_depth: number
  last_seq: number
  restarts: number
  agent_version?: string
  spend?: AgentSpend
  autonomy?: AgentAutonomy
}

export type AgentStatus = {
  sandbox_id: string
  agent: {
    id: string
    name: string
    /** False when the agent CLI has no resume command, so every message starts a new conversation. */
    remembers?: boolean
    memory_note?: string
  }
  session: AgentSession
}

export type AgentModels = {
  supported: boolean
  models: string[]
  current?: string
  /** Why listing is unavailable; shown instead of an empty picker. */
  reason?: string
}

export type AgentThread = {
  id: string
  title?: string
  turns: number
  first_at: string
  last_at: string
}

const agentBase = (id: string) => `/v1/sandboxes/${id}/agent`

export const getAgentStatus = (id: string) =>
  request<AgentStatus>('GET', `${agentBase(id)}/status`)

// Served from the sandbox when up, from the control plane's mirror otherwise. `note` says
// why the mirror answered and is rendered verbatim.
export const listAgentTurns = (id: string) =>
  request<{ turns: AgentTurn[]; served_from?: 'node' | 'mirror'; note?: string }>(
    'GET', `${agentBase(id)}/turns`)

export const submitAgentTurn = (
  id: string,
  message: string,
  opts: { mode?: string; priority?: boolean } = {},
) =>
  request<{ turn_id: string; state: string; seq: number; woke_sandbox: boolean }>(
    'POST', `${agentBase(id)}/turns`, { message, ...opts },
  )

export const interruptAgentTurn = (id: string, turnID: string, graceSeconds = 5) =>
  request<{ state: string }>('POST', `${agentBase(id)}/turns/${turnID}/interrupt`,
    { grace_seconds: graceSeconds })

export const dequeueAgentTurn = (id: string, turnID: string) =>
  request<{ removed: boolean }>('DELETE', `${agentBase(id)}/turns/${turnID}`)

export const reorderAgentQueue = (id: string, order: string[]) =>
  request<{ reordered: boolean }>('POST', `${agentBase(id)}/queue/reorder`, { order })

export const getAgentModels = (id: string) =>
  request<AgentModels>('GET', `${agentBase(id)}/models`)

export const listAgentThreads = (id: string) =>
  request<{ threads: AgentThread[] }>('GET', `${agentBase(id)}/threads`)

export const getAgentBudget = (id: string) =>
  request<{ enabled: boolean; budget?: AgentSpend; reason?: string }>(
    'GET', `${agentBase(id)}/budget`)

export const setAgentBudget = (id: string, capUSD: number) =>
  request<{ enabled: boolean; budget?: AgentSpend }>(
    'PATCH', `${agentBase(id)}/budget`, { cap_usd: capUSD })

/** Cursor-based so a reconnect resumes rather than replays. */
export const agentEventsURL = (id: string, sinceSeq: number) =>
  `${agentBase(id)}/events?since_seq=${sinceSeq}`

// Every field is either usable or says why not; the card must not hide what a deployment
// cannot do.
export type AgentConnectBundle = {
  sandbox_id: string
  agent: { id: string; name: string }
  token: {
    id: string
    scopes: string[]
    /** Shown once; a later call mints a different token and returns null here. */
    value: string | null
    shown_once: boolean
    expires_at: string
    note: string
  }
  endpoints: Record<string, unknown>
  snippets: Record<string, string>
  mcp: { config: string; transport: string; note?: string; ready?: boolean }
  channels?: Array<{ id?: string; name?: string; status?: string; reason?: string }>
  origin: { url: string; note: string }
}

// Each call mints a credential, so call on demand, never on tab render.
export const getAgentConnect = (id: string) =>
  request<AgentConnectBundle>('GET', `${agentBase(id)}/connect`)

/** IsolationFact.state is three-valued: an unmeasurable boundary renders as unknown, not as a false no. */
export type IsolationFact = {
  label: string
  state: 'yes' | 'no' | 'unknown'
  detail: string
}

// Every fact is measured server-side; the card renders what it is given, including no and unknown.
export const getAgentIsolation = (id: string) =>
  request<{ sandbox_id: string; facts: IsolationFact[] }>('GET', `${agentBase(id)}/isolation`)

// Approvals only exist for gated agents. Without a gate the endpoint answers 501, which
// must not be rendered as an empty queue.

export type AgentApproval = {
  id: string
  turn_id: string
  tool: string
  tool_use_id?: string
  /** The category a remembered decision applies to, e.g. `Bash: npm install *`; shown verbatim. */
  signature: string
  /** sha256 of the arguments; not reversible. */
  args_digest: string
  /** Redacted and truncated; raw arguments are never stored or sent. */
  args_preview: string
  requested_at: string
  decision?: 'allow' | 'ask'
  decided_by?: string
  remember?: string
  reason?: string
}

export type AgentApprovalsResponse = {
  approvals: AgentApproval[]
  resolved?: AgentApproval[]
  remember_scopes: string[]
  timeout_seconds: number
  /** Always "deny", never allow. */
  on_timeout: string
  /** False for automation and for tokens without the scope. */
  can_decide?: boolean
  cannot_decide_reason?: string
}

export type AgentPolicy = {
  rules: { tool: string; argv?: string[]; forbid?: string[]; path_prefix?: string; why: string }[]
  default: string
  note: string
  /** Standing grants with scope: session grants vanish on agent restart, sandbox grants must be revoked. */
  remembered?: { signature: string; scope: string }[]
}

export const listAgentApprovals = (id: string) =>
  request<AgentApprovalsResponse>('GET', `${agentBase(id)}/approvals`)

export const decideAgentApproval = (
  id: string, approvalID: string,
  decision: 'allow' | 'deny',
  remember: 'once' | 'session' | 'sandbox' = 'once',
) =>
  request<{ decided: string; decision: string }>(
    'POST', `${agentBase(id)}/approvals/${encodeURIComponent(approvalID)}`,
    { decision, remember })

export const getAgentPolicy = (id: string) =>
  request<AgentPolicy>('GET', `${agentBase(id)}/policy`)

export const forgetAgentApproval = (id: string, signature: string) =>
  request<{ forgot: string }>('DELETE', `${agentBase(id)}/policy/remembered`, { signature })

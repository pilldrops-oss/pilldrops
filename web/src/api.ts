import type {
  Volume,
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
  PreviewStatus,
  LimitsInfo,
  VaultSecret,
  CustomDomain,
  Capabilities,
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

// bearerHeader is the Authorization header for key; none at all for no key,
// since an empty "Bearer " reads as a malformed credential to some servers.
function bearerHeader(key: string): Record<string, string> {
  return key ? { Authorization: `Bearer ${key}` } : {}
}

// No key (make dev's anonymous server) sends no Authorization header at all.
async function authHeaders(): Promise<HeadersInit> {
  return { ...bearerHeader(await bearerToken()), 'Content-Type': 'application/json' }
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

// PreviewAccessResp says what the flip did; `was` is the value before this call.
export type PreviewAccessResp = { id: string; public_preview: boolean; was: boolean }

// setPreviewAccess flips whether the app preview needs a token. Only the sandbox's
// creator or an operator may call it; anyone else gets 404, same as the visibility
// flip. The terminal (7681) and desktop (6080) ports stay token-gated either way.
export const setPreviewAccess = (id: string, pub: boolean) =>
  request<PreviewAccessResp>('POST', `/v1/sandboxes/${id}/preview-access`, { public: pub })

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
// The orchestrator names the lifecycle `state`, a node names it `status`.
export const listSandboxes = () =>
  request<{ sandboxes: (SandboxRow & { state?: string })[] | null }>('GET', '/v1/sandboxes', undefined).then(r => ({
    sandboxes: (r.sandboxes ?? []).map(s => ({ ...s, status: s.status ?? s.state ?? '' })),
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

export const fsDownload = async (id: string, path: string): Promise<Blob> => {
  const key = await bearerToken()
  const r = await fetch(`/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, {
    headers: bearerHeader(key),
  })
  if (!r.ok) {
    // The server's own message is what says *why* — a 404 naming the missing
    // path reads very differently from a bare status.
    const body = await r.text().catch(() => '')
    throw new Error(`GET /v1/sandboxes/${id}/files → ${r.status}: ${body}`)
  }
  return r.blob()
}

export const fsRead = async (id: string, path: string): Promise<string> => (await fsDownload(id, path)).text()

// content may be a Blob (a File from a drop or a picker), which is sent as-is
// rather than read into a string first: /files streams the body to the sandbox
// under its own PD_FILES_MAX_BYTES, so a file far larger than memory-as-text
// can go through.
export const fsWrite = async (id: string, path: string, content: string | Blob): Promise<void> => {
  const key = await bearerToken()
  const r = await fetch(`/v1/sandboxes/${id}/files?path=${encodeURIComponent(path)}`, {
    method: 'POST',
    headers: {
      ...bearerHeader(key),
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

// IS_LOCAL_RUNTIME is make dev's in-process runtime: plain processes, so no
// pause, fork, sharing or custom domains.
export const IS_LOCAL_RUNTIME = window.__SANDBOXD__?.runtime === 'local'

export const listTemplates = () => request<Template[]>('GET', '/v1/templates', undefined)

// listPublicTemplates is the same catalog unauthenticated, the signed-out fallback.
export const listPublicTemplates = () =>
  request<Template[]>('GET', '/v1/templates/public', undefined, false)

// RevisedPlanResp is the server-validated reply from the BYOK plan reviser.
export interface RevisedPlanResp {
  services: Array<{ template: string; reason: string }>
  note: string
}


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

export const listSecrets = () => request<VaultSecret[]>('GET', '/v1/secrets', undefined)
// auto_inject is omitted to keep the current setting on a rotation; the server
// only resets it to false for a brand-new secret, or when this is passed explicitly.
export const putSecret = (name: string, value: string, autoInject?: boolean) =>
  request<VaultSecret>('POST', '/v1/secrets', autoInject === undefined ? { name, value } : { name, value, auto_inject: autoInject })
export const deleteSecret = (name: string) =>
  request<void>('DELETE', `/v1/secrets/${encodeURIComponent(name)}`, undefined)

// setSecretAutoInject flips a secret's auto-inject flag without touching its value.
export const setSecretAutoInject = (name: string, autoInject: boolean) =>
  request<{ name: string; auto_inject: boolean }>('PATCH', `/v1/secrets/${encodeURIComponent(name)}`, { auto_inject: autoInject })

export const getCapabilities = () =>
  request<Capabilities>('GET', '/v1/capabilities', undefined)

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

// Standalone volumes: disks that outlive the sandbox they are attached to.
export const listVolumes = () => request<{ volumes: Volume[] | null }>('GET', '/v1/volumes', undefined).then(r => r.volumes ?? [])
export const createVolume = (name: string, sizeMB: number) => request<Volume>('POST', '/v1/volumes', { name, size_mb: sizeMB })
export const deleteVolume = (id: string) => request<void>('DELETE', `/v1/volumes/${encodeURIComponent(id)}`, undefined)

declare global {
  interface Window {
    __SANDBOXD__: { key: string; runtime?: string; clustered?: boolean; mode?: 'user' | 'admin'; sandboxDomain?: string; posthogKey?: string }
  }
}

export interface TemplateStat {
  template: string
  active: number
}

export interface StatsPayload {
  active_count: number
  max_active: number
  template_stats: Record<string, TemplateStat>
  templates?: string[]
  [key: string]: unknown
}

export interface SandboxRow {
  id: string
  template: string
  /** Optional human label; render `name || id`. */
  name?: string
  /** Optional DNS label: the sandbox is also https://<slug>.<domain>/ and any id argument accepts it. */
  slug?: string
  status: string
  boot_type: string
  boot_ms: number
  created_at: string
  expires_at: string
  /** App port published at create/deploy time; 0/absent = none declared. */
  public_port?: number
  /** Who may see it: "org" (everyone in the org, the default) or "private" (the creating key and operators). */
  visibility?: 'org' | 'private'
  /** The API key that created it; "" for sandboxes made before visibility existed. */
  created_by?: string
  /** Owning node id in the aggregate cluster view. */
  node?: string
  /** Preview token; append as ?token= to the preview URL. */
  cdp_token?: string
  /**
   * Whether this sandbox can serve a screen on the desktop port: the node's
   * runtime can host an X server AND this sandbox's image holds the software.
   * Absent from a node too old to report it, which is not the same as false —
   * treat only an explicit `false` as "no desktop".
   */
  has_desktop?: boolean
}

export interface LogEntry {
  level: string
  time: string
  msg: string
  fields?: Record<string, unknown>
}

/** ConcPoint is one concurrency sample: unix seconds, running count. */
export interface ConcPoint {
  at: number
  n: number
}

export interface EventRow {
  sandbox_id: string
  event: string // created | destroyed | paused | resumed
  template?: string
  active_after: number
  at: number
}

/** NodeInfo is one sandboxd host from GET /v1/nodes. */
export interface NodeInfo {
  id: string
  url: string
  zone: string
  labels?: Record<string, string>
  healthy: boolean
  status: string
  first_seen: number
  last_seen: number
  capacity: {
    cpu_cores: number
    cpu_percent: number
    load1: number
    mem_total_mb: number
    mem_available_mb: number
    mem_committed_mb: number
    disk_free_gb: number
    sandboxes_active: number
    vcpu_committed?: number
  }
}

export interface ZoneStat {
  zone: string
  nodes: number
  cores: number
  mem_free_gb: number
  sandboxes_active: number
  headroom_sandboxes: number
  status: string
}

export interface ScaleRecommendation {
  level: string
  message: string
  suggested_nodes: number
}

/** ClusterStats is GET /v1/cluster/stats. */
export interface ClusterStats {
  nodes_total: number
  nodes_healthy: number
  nodes_available: number
  nodes_full: number
  nodes_cordoned: number
  nodes_unreachable: number
  cores_total: number
  mem_total_gb: number
  mem_used_gb: number
  mem_free_gb: number
  sandboxes_active: number
  headroom_sandboxes: number
  zones: ZoneStat[]
  recommendation: ScaleRecommendation
}

/** JoinToken is POST /v1/join-tokens: a one-time token plus the bootstrap command to paste on a new host. */
export interface JoinToken {
  token: string
  expires_at: number // unix seconds
  bootstrap_cmd: string
}

/** ImageInfo is an admin-curated image from GET /v1/images. */
export interface ImageInfo {
  id: string
  name: string
  description: string
  port: number
  vcpu: number
  memory_mb: number
  size_mb: number
  published: boolean
  featured: boolean
  language: string
  tags: string[]
  updated_at: number
}

/** PatchImageReq is the PATCH /v1/images/{id} body. */
export interface PatchImageReq {
  published?: boolean
  featured?: boolean
  name?: string
  description?: string
}

/** HostResp is GET /v1/host: live resources of the physical machine. */
export interface HostResp {
  cpu_cores: number
  cpu_percent: number
  load_avg: [number, number, number]
  mem_total_mb: number
  mem_used_mb: number
  mem_available_mb: number
  disk_total_gb: number
  disk_used_gb: number
  disk_free_gb: number
  sandboxes_active: number
  vcpu_committed: number
  mem_committed_mb: number
}

/**
 * CustomDomain is one row of GET /v1/domains: a customer hostname in front of a
 * public sandbox. `status` is 'pending' until the platform has resolved the DNS
 * record itself; only 'active' rows route, and `record` is the line to create.
 */
export interface CustomDomain {
  host: string
  sandbox_id: string
  port: number
  wildcard: boolean
  status: 'pending' | 'active'
  /** The resolver's own reason the host is not active yet; '' when it is. */
  error: string
  record: string
  cname_target: string
  created_at: number
}

/** VaultSecret is GET /v1/secrets: name and last4 only, never the value. */
export interface VaultSecret {
  name: string
  last4: string
  /** Present only for non-credential settings (gateway URL, model); never for API keys. */
  value?: string
  created_at: string
  updated_at: string
}

export interface CreateSandboxReq {
  /** Optional human label. */
  name?: string
  /** Vault secret names resolved to env vars server-side; raw values never appear here. */
  secret_refs?: string[]
  /** Agent bootstrap args, expanded server-side from the template manifest. */
  args?: { task?: string; access_mode?: string }
  template: string
  timeout?: number
  vcpu?: number
  memory_mb?: number
  storage_mb?: number
  files?: Array<{ path: string; content: string }>
  setup?: string[]
  /** Return the id immediately and run files/setup/start behind it; progress via GET /v1/sandboxes/{id}/deploy. */
  async_setup?: boolean
  start?: string
  port?: number
  /** Isolation runtime ("firecracker" | "docker"); omit for auto. */
  runtime?: string
  /** Environment variables as "NAME=VALUE" strings. */
  env?: string[]
  /** Autonomy posture for an agent workstation. Advisory: a "guided" launch whose approval gate fails reports autonomous. */
  agent_mode?: 'supervised' | 'guided' | 'autonomous'
  /** Idle auto-pause window in seconds; -1 = never pause. */
  idle_timeout?: number
  /** Make the preview reachable without a token; default private. */
  public?: boolean
}

/** DeployStep is one command a deploy ran. */
export interface DeployStep {
  cmd: string
  /** Exit status; -1 when the command could not be run at all. */
  exit: number
  err?: string
  ms: number
}

/** DeployProgress is GET /v1/sandboxes/{id}/deploy. 'unknown' means this node never heard of the deploy; it is not done. */
export interface DeployProgress {
  state: 'running' | 'done' | 'failed' | 'unknown'
  steps: DeployStep[]
  log: string[]
}

export interface CreateSandboxResp {
  id: string
  template: string
  status: string
  created_at: string
  expires_at: string
  app_url?: string
  /** Preview link when the template publishes a built-in app port. */
  url?: string
  deploy_log?: string
  /** Present when async_setup was requested. */
  deploy_state?: string
  /** Preview token, bound to cdp_token_port only; other ports need their own token (lib/previewToken). */
  cdp_token?: string
  cdp_token_port?: number
  /** Browser template: ready-to-use tokened links. */
  viewer_url?: string
  cdp_url?: string
}

export interface ExecResp {
  stdout: string
  stderr: string
  exit_code: number
  truncated: boolean
}

export interface StatusResp {
  status: string
}

export interface ApiKey {
  id: string
  name: string
  masked_key: string
  // Unix seconds from /v1/keys; string form kept for older callers.
  last_used: number | string
  created_at: number | string
}

export interface CreateKeyResp {
  id: string
  name: string
  key: string
  masked_key: string
}

export interface UsagePoint {
  time: number // unix seconds
  concurrent: number
  start_rate: number
}

export interface SandboxLogsResp {
  lines: string[]
  truncated?: boolean
  note?: string
}

export interface UsageResp {
  total_starts: number
  vcpu_hours: number
  ram_gb_hours: number
  points: UsagePoint[]
  /** Client tag (web/cli/mcp/sdk/api/unknown) to count. */
  by_client?: Record<string, number>
}

export interface TemplateEnv {
  name: string
  value?: string
  required?: boolean
  secret?: boolean
}

export interface Template {
  id: string
  name: string
  description: string
  language?: string
  icon?: string
  tags?: string[]
  cpu: number
  memory_mb: number
  storage_mb?: number
  visibility: string
  /** Port of the template's built-in app. */
  port?: number
  /** Docker image for image-backed services; no build. */
  image?: string
  ports?: number[]
  env?: TemplateEnv[]
  /** database | queue | language | sandbox */
  category?: string
  /** Seeded catalog entries are protected from deletion. */
  builtin?: boolean
  dockerfile?: string
  /** Key tools baked into the image. */
  preinstalled?: string[]
  /** Start command (may use $PORT); "" for shell/server images. */
  start_suggestion?: string
  /** Pre-start install commands. */
  setup_suggestion?: string[]
  /** True when the image ships a built-in server on the preview URL. */
  has_server?: boolean
  server_desc?: string
  /** Preview port to prefill. */
  default_port?: number
  docs_url?: string
  /** Launch manifest purpose: shell|code|app|agent|screen|test|verdict|service. Drives the launch form. */
  kind?: string
  /** The one input quick-launch asks for; absent means zero-field launch. */
  primary_input?: TemplatePrimaryInput
  /** Surface a launch lands on: terminal|preview|screen|report|verdict|endpoint|connection. */
  outcome?: string
  /** Agent-CLI metadata for kind=agent templates. */
  agent?: TemplateAgentMeta
}

export interface TemplatePrimaryInput {
  /** code | url | target | apk | script | tests | task */
  type: string
  label: string
  required?: boolean
}

/** TemplateAgentMeta mirrors Go AgentMeta. */
export interface TemplateAgentMeta {
  key_env?: string
  key_label?: string
  key_note?: string
  run_cmd?: string
  task_placeholder?: string
  cli_bin?: string
  install_cmd?: string
  run_flags?: string
  base_url_env?: string
  model_env?: string
  config_file?: string
  config_label?: string
  config_format?: string
  config_example?: string
  /** Flag routing tool approvals through the gate. Empty means the agent cannot be gated, so Guided must be offered as unavailable. */
  permission_tool?: string
}

export interface FilesystemEntry {
  name: string
  is_dir: boolean
  size: number
  modified: string
}

export interface FilesystemListResp {
  entries: FilesystemEntry[]
}

export interface SandboxDetail {
  id: string
  template: string
  /** Optional human label. */
  name?: string
  /** Optional DNS label: the sandbox is also https://<slug>.<domain>/. */
  slug?: string
  status: string
  boot_type: string
  boot_ms: number
  created_at: string
  expires_at: string
  /** App port published at create/deploy time; 0/absent = none declared. */
  public_port?: number
  /** Preview token; append as ?token= to the preview URL. */
  cdp_token?: string
  /** Who may see it: "org" (everyone in the org, the default) or "private" (the creating key and operators). */
  visibility?: 'org' | 'private'
  /** The API key that created it; "" for sandboxes made before visibility existed. */
  created_by?: string
  /** Storage cap, MB. */
  storage_mb?: number
  /** Measured disk usage, MB; absent when the runtime cannot report it. */
  storage_used_mb?: number
}

export interface FunctionInfo {
  name: string
  template: string
  start: string
  port: number
  vcpu: number
  memory_mb: number
  max_replicas: number
  replicas: number
  sandbox_id: string
  invokes: number
  cold_starts: number
  last_invoked_at: number
  env: Record<string, string>
  created_at: number
  updated_at: number
}

export interface RegisterFunctionReq {
  name: string
  template: string
  start: string
  port: number
  vcpu?: number
  memory_mb?: number
  max_replicas?: number
  env?: Record<string, string>
  files: { path: string; content: string }[]
}

export interface UpdateFunctionReq {
  template: string
  start: string
  port: number
  env?: Record<string, string>
  files?: { path: string; content: string }[]
}

export interface InvokeResult {
  status: number
  body: string
  cold_start: boolean
  duration_ms: number
  headers: Record<string, string>
}

export interface FunctionMetrics {
  invokes: number
  cold_starts: number
  last_invoked_at: number
  state: string
  vcpu_seconds: number
  ram_gb_seconds: number
  uptime_seconds: number
}

export interface FunctionCode {
  files: { path: string; content: string }[]
}

export interface InvokeOptions {
  method?: string
  path?: string
  headers?: Record<string, string>
  body?: string
}


export interface Plan {
  id: string
  name: string
  price_monthly: number
  allowance: number
  max_concurrent: number
  session_ttl_hrs: number
  max_vcpu: number
  max_mem_mb: number
  max_storage_mb: number
  self_subscribe: boolean
  persistent: boolean
  included_hours: number
  max_workstations: number
}

/** LimitsInfo is GET /v1/limits: caps and consumption, no money. */
export interface LimitsInfo {
  plan: Plan
  hours?: { used: number; included: number; remaining: number; pct: number }
  /** `measured:false` means the runtime cannot report; render "n/a", never 0. */
  storage?: { used_mb: number; cap_mb: number; sandboxes: number; measured: boolean }
  plans: Plan[]
}

export interface AdminUser {
  owner_id: string
  email: string
  first_seen: number
  last_seen: number
  login_count: number
  active_sandboxes: number
  total_created: number
  functions: number
  plan: string
}

/** AdminSandbox mirrors store.ActiveSandboxRow's JSON tags (snake_case, env-free). */
export interface AdminSandbox {
  id: string
  template: string
  org_id: string
  state: string
  start_cmd?: string
  source?: string
  public_port?: number
  vcpu: number
  mem_mib: number
  created_at: number
  expires_at: number
}
export interface AdminHistory { id: string; template: string; exit_state: string; created_at: number; destroyed_at: number }
export interface AdminFunction { Name: string; Template: string; Invokes: number; ColdStarts: number; LastInvokedAt: number }
export interface AdminUsage { VCPUSeconds: number; RAMGBSeconds: number }
export interface AdminEvent { sandbox_id: string; event: string; template?: string; at: number }
export interface AdminAudit { key_id: string; action: string; target: string; at: number }

export interface AdminUserDetail {
  user: AdminUser
  active: AdminSandbox[]
  history: AdminHistory[]
  functions: AdminFunction[]
  usage: AdminUsage
  events: AdminEvent[]
  audit: AdminAudit[]
  /** Client tag (web/cli/mcp/sdk/api/unknown) to count. */
  by_client?: Record<string, number>
}

export interface NameCount { name: string; count: number }
export interface DayCount { day: number; count: number } // day = unix / 86400
export interface AdminErrorRow { id: string; org_id: string; template: string; exit_state: string; destroyed_at: number }
export interface FeedbackRow { id: string; name: string; email: string; kind: string; message: string; source: string; ip: string; ts: number }
export interface WaitlistRow { email: string; source: string; note: string; created_at: number }
export interface AdminEventRow { org_id: string; sandbox_id: string; event: string; template: string; at: number }
export interface AdminOverview {
  total_users: number
  active_users_24h: number
  logins: number
  active_sandboxes: number
  total_created: number
  total_functions: number
  crashed_count: number
  templates: NameCount[]
  exit_states: NameCount[]
  by_source: NameCount[]
  created_by_day: DayCount[]
  recent_errors: AdminErrorRow[]
  recent_activity: AdminEventRow[]
}

/** AdminAuditDetail is the parsed audit `detail` payload; fields vary by action. */
export interface AdminAuditDetail {
  // sandbox.create
  template?: string; image?: string; vcpu?: number; mem_mb?: number; storage_mb?: number
  timeout?: number; idle_timeout?: number; ports?: number[]; port?: number
  start_cmd?: string; setup?: string[]; env_keys?: string[]
  files?: { path: string; size: number }[]
  client?: string; user_agent?: string
  // sandbox.exec
  cmd?: string; exit_code?: number; duration_ms?: number
  // sandbox.fs.*
  path?: string; src?: string; dst?: string; size?: number
  // sandbox.destroy / sandbox.timeout
  reason?: string; timeout_secs?: number
}

/** AdminActivityRow is one timeline row; `detail` is absent for pre-tracking rows. */
export interface AdminActivityRow { at: number; key_id: string; org_id: string; action: string; detail?: AdminAuditDetail }

/** AdminSandboxRecord is a history row (destroyed) or a live row (active); fields not shared are optional. */
export interface AdminSandboxRecord {
  id: string; template: string; org_id: string; source?: string
  vcpu?: number; mem_mib?: number; created_at?: number
  // destroyed
  image?: string; node?: string; owner?: string; exit_state?: string
  destroyed_at?: number; duration_secs?: number
  vcpu_seconds?: number; ram_gb_seconds?: number
  /** Creation-client tag; history records only. */
  client?: string
  // active
  state?: 'running' | 'paused'; start_cmd?: string; public_port?: number; expires_at?: number
}

/**
 * AdminSandboxDetail is GET /v1/admin/sandboxes/{id}. `state` is the lifecycle,
 * `record.state` the runtime state; record is null only when state is 'unknown'.
 */
export interface AdminSandboxDetail {
  record: AdminSandboxRecord | null
  state: 'active' | 'destroyed' | 'unknown'
  launch?: AdminAuditDetail
  activity: AdminActivityRow[]
  logs: string
  logs_note?: string
}

export interface LoginEvent { owner_id: string; email: string; at: number }
export interface AdminSessions {
  total: number
  logins_24h: number
  logins_7d: number
  unique_users: number
  by_day: DayCount[]
  recent: LoginEvent[]
}

/** HistoryRecord is a destroyed sandbox; `logs` is only populated by the detail GET. */
export interface HistoryRecord {
  id: string
  org_id: string
  owner?: string
  template: string
  image?: string
  node?: string
  exit_state: string
  created_at: number
  destroyed_at: number
  duration_secs: number
  vcpu: number
  mem_mib: number
  vcpu_seconds?: number
  ram_gb_seconds?: number
  logs?: string
}

/** EnvEntry is one env var as the settings page sees it; secret values never leave the server. */
export interface EnvEntry {
  name: string
  /** Present only when the variable is not a credential. */
  value?: string
  secret: boolean
  set: boolean
}

export interface SandboxConfig {
  env: EnvEntry[]
  model: string
  base_url: string
}

/** McpServer is a catalogue entry: a process to run (command) or an endpoint to reach (url). */
export interface McpServer {
  id: string
  name: string
  description: string
  category: string
  command?: string
  args?: string[]
  url?: string
  /** Credentials this server needs. */
  env_keys?: string[]
}

export interface CronJob {
  id: string
  sandbox_id: string
  name: string
  schedule: string
  timezone: string
  command: string
  enabled: boolean
  next_run_at: number
  last_run_at: number
  /** "ok" | "failed" (non-zero exit) | "error" (machine unreachable). */
  last_status: string
  last_output: string
}

export interface ChatMessage {
  role: 'user' | 'agent'
  text: string
  at: number
}

/**
 * PreviewState is GET /v1/sandboxes/:id/preview-status. 'starting' means the
 * app has not bound its port yet; 'unknown' means readiness could not be
 * probed and callers must not infer success or failure.
 */
export type PreviewState =
  | 'ready'
  | 'starting'
  | 'no_page'
  | 'app_error'
  | 'terminal'
  | 'paused'
  | 'unpublished'
  | 'unknown'

export interface PreviewStatus {
  state: PreviewState
  sandbox: string
  /** Absent on `unknown`, which is emitted without probing. */
  port?: number
  /** Absent when the probe itself failed to complete. */
  upstream_status?: number
}

/**
 * SandboxMetrics is GET /v1/sandboxes/:id/metrics. vcpu/mem_mib are the cap;
 * the optional gauges are absent when the runtime cannot sample them. Render
 * absent as a dash, never 0.
 */
export interface SandboxMetrics {
  id: string
  state: string
  vcpu: number
  mem_mib: number
  vcpu_seconds: number
  ram_gb_seconds: number
  uptime_seconds: number
  cpu_pct?: number
  mem_used_mib?: number
  disk_used_mb?: number
  disk_limit_mb?: number
}

/** PortStatus is one probe from GET /v1/sandboxes/:id/ports; a fixed candidate list, not a scan. */
export interface PortStatus {
  port: number
  listening: boolean
}

export interface SandboxPorts {
  ports: PortStatus[]
}

/** Project mirrors internal/store's Project. */
export interface Project {
  id: string
  org_id: string
  owner?: string
  name: string
  /** A serialised Plan, opaque over the wire. */
  plan?: string
  created_at: number
  last_run_at?: number
  archived?: boolean
}

export interface RunService {
  service_id: string
  sandbox_id: string
  verdict?: string
}

export interface ProjectRun {
  id: string
  project_id: string
  org_id: string
  /** Frozen at execution; editing the project's plan never rewrites it. */
  plan_snapshot?: string
  services?: RunService[]
  started_at: number
  ended_at?: number
}

/** ChannelInfo is one way to reach a workstation; unsupported channels are listed with `available:false` and a reason. */
export interface ChannelInfo {
  id: string
  name: string
  available: boolean
  reason?: string
  linked: boolean
  chat_id?: string
  read_only?: boolean
  setup?: string
}

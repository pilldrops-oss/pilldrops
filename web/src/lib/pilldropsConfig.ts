// The committed file declaring a repo's sandbox configuration. Reader and writer live
// together so they cannot drift.

export const PILLDROPS_FILE = 'pilldrops.json'

/** PILLDROPS_VERSION is bumped when the shape changes incompatibly. */
export const PILLDROPS_VERSION = 1

export interface PilldropsConfig {
  version: number
  template?: string
  resources?: { vcpu?: number; memory_mb?: number; storage_mb?: number }
  setup?: string[]
  start?: string
  port?: number
  /** Env var names the sandbox needs, never values; the file is committed to git. */
  env?: string[]
}

export type ParseResult =
  | { ok: true; config: PilldropsConfig }
  | { ok: false; error: string }

const fail = (msg: string): ParseResult => ({ ok: false, error: msg })

/** parsePilldropsConfig validates the file; every failure names it rather than silently ignoring it. */
export function parsePilldropsConfig(raw: string, knownTemplateIds: string[]): ParseResult {
  let obj: unknown
  try {
    obj = JSON.parse(raw)
  } catch {
    return fail(`${PILLDROPS_FILE} isn’t valid JSON, so it was ignored.`)
  }
  if (typeof obj !== 'object' || obj === null || Array.isArray(obj)) {
    return fail(`${PILLDROPS_FILE} should contain a JSON object.`)
  }
  const o = obj as Record<string, unknown>

  const version = typeof o.version === 'number' ? o.version : 1
  if (version !== PILLDROPS_VERSION) {
    return fail(`${PILLDROPS_FILE} has version ${version}, which this dashboard doesn’t understand.`)
  }

  const config: PilldropsConfig = { version: PILLDROPS_VERSION }

  if (o.template !== undefined) {
    if (typeof o.template !== 'string') {
      return fail(`${PILLDROPS_FILE}: "template" should be a template id.`)
    }
    if (!knownTemplateIds.includes(o.template)) {
      return fail(`${PILLDROPS_FILE} names the template "${o.template}", which isn’t in the catalog.`)
    }
    config.template = o.template
  }

  // Names only: accepting values would invite credentials into git. Never echo a value back.
  if (o.env !== undefined) {
    if (!Array.isArray(o.env) || o.env.some(v => typeof v !== 'string')) {
      return fail(
        `${PILLDROPS_FILE}: "env" must be a list of variable NAMES, not values — ` +
        `this file is committed to your repository. Store values in the Vault instead.`,
      )
    }
    config.env = o.env as string[]
  }

  if (o.setup !== undefined) {
    if (!Array.isArray(o.setup) || o.setup.some(v => typeof v !== 'string')) {
      return fail(`${PILLDROPS_FILE}: "setup" should be a list of commands.`)
    }
    config.setup = o.setup as string[]
  }

  if (o.start !== undefined) {
    if (typeof o.start !== 'string') return fail(`${PILLDROPS_FILE}: "start" should be a command string.`)
    config.start = o.start
  }

  if (o.port !== undefined) {
    if (typeof o.port !== 'number' || o.port <= 0 || o.port > 65535) {
      return fail(`${PILLDROPS_FILE}: "port" should be a number between 1 and 65535.`)
    }
    config.port = o.port
  }

  if (o.resources !== undefined) {
    if (typeof o.resources !== 'object' || o.resources === null || Array.isArray(o.resources)) {
      return fail(`${PILLDROPS_FILE}: "resources" should be an object.`)
    }
    const r = o.resources as Record<string, unknown>
    const res: NonNullable<PilldropsConfig['resources']> = {}
    for (const k of ['vcpu', 'memory_mb', 'storage_mb'] as const) {
      if (r[k] !== undefined) {
        if (typeof r[k] !== 'number' || (r[k] as number) <= 0) {
          return fail(`${PILLDROPS_FILE}: "resources.${k}" should be a positive number.`)
        }
        res[k] = r[k] as number
      }
    }
    if (Object.keys(res).length) config.resources = res
  }

  return { ok: true, config }
}

/** DetectionLike is the structural subset of lib/detect's Detection; `setup` is one string here, string[] in the config. */
export interface DetectionLike {
  templateId?: string
  start?: string
  setup?: string
  port?: number
}

export interface MergedLaunchFields {
  templateId?: string
  start?: string
  setup?: string
  port?: number
  /** Which side supplied each defined field; absent when neither did. */
  source: Partial<Record<'templateId' | 'start' | 'setup' | 'port', 'pilldrops.json' | 'detection'>>
}

/** mergeConfigOverDetection lets the committed file win over detection, field by field. */
export function mergeConfigOverDetection(
  config: PilldropsConfig | null,
  detection: DetectionLike | null,
): MergedLaunchFields {
  const out: MergedLaunchFields = { source: {} }

  const templateId = config?.template ?? detection?.templateId
  if (templateId !== undefined) {
    out.templateId = templateId
    out.source.templateId = config?.template !== undefined ? 'pilldrops.json' : 'detection'
  }

  // The config's setup is string[]; the form expects one string.
  const configSetup = config?.setup?.length ? config.setup.join('\n') : undefined
  const setup = configSetup ?? detection?.setup
  if (setup !== undefined) {
    out.setup = setup
    out.source.setup = configSetup !== undefined ? 'pilldrops.json' : 'detection'
  }

  const start = config?.start ?? detection?.start
  if (start !== undefined) {
    out.start = start
    out.source.start = config?.start !== undefined ? 'pilldrops.json' : 'detection'
  }

  const port = config?.port ?? detection?.port
  if (port !== undefined) {
    out.port = port
    out.source.port = config?.port !== undefined ? 'pilldrops.json' : 'detection'
  }

  return out
}

/** serialisePilldropsConfig writes the committable form: indented, newline-terminated, empty fields omitted. */
export function serialisePilldropsConfig(c: PilldropsConfig): string {
  const out: Record<string, unknown> = { version: c.version || PILLDROPS_VERSION }
  if (c.template) out.template = c.template
  if (c.resources && Object.keys(c.resources).length) out.resources = c.resources
  if (c.setup?.length) out.setup = c.setup
  if (c.start) out.start = c.start
  if (c.port) out.port = c.port
  if (c.env?.length) out.env = c.env
  return `${JSON.stringify(out, null, 2)  }\n`
}

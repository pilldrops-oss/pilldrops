// Vault fields per provider: API key, plus gateway base URL and model where a
// consumer (`configure` = portal AI call, `launch` = sandbox env) reads one.
// Launch env names are duplicated from AGENT_CONFIG; importing it forms a cycle.

export type BaseUrlUse = 'configure' | 'launch'

export interface ProviderSetting {
  label: string
  /** Vault name for the API key. */
  keyEnv: string
  hint: string
  usedBy: string
  /** Vault name for the gateway base URL; absent when nothing reads one. */
  baseUrlEnv?: string
  /** Vault name for the model override; absent when nothing reads one. */
  modelEnv?: string
  /** Endpoint used when no gateway is set. */
  defaultBase?: string
  modelHint?: string
  /** Who consumes the base URL; drives the explanation in the UI. */
  baseUrlUse?: BaseUrlUse[]
  /** Shown when a provider has no base URL field. */
  note?: string
}

// The portal reads exactly these two from the vault for its own AI calls.
const CONFIGURE_BASE_URLS: Record<string, { baseUrlEnv: string; modelEnv: string; defaultBase: string; modelHint: string }> = {
  ANTHROPIC_API_KEY: {
    baseUrlEnv: 'ANTHROPIC_BASE_URL',
    modelEnv: 'ANTHROPIC_MODEL',
    defaultBase: 'https://api.anthropic.com',
    modelHint: 'claude-sonnet-4-5',
  },
  OPENAI_API_KEY: {
    baseUrlEnv: 'OPENAI_BASE_URL',
    modelEnv: 'OPENAI_MODEL',
    defaultBase: 'https://api.openai.com',
    modelHint: 'gpt-4o-mini',
  },
}

interface Seed {
  label: string
  keyEnv: string
  hint: string
  usedBy: string
  /** Mirrors AGENT_CONFIG[agentId].baseUrlEnv; empty when the CLI reads none. */
  launchBaseUrlEnv?: string
  launchModelEnv?: string
  defaultBase?: string
  modelHint?: string
  note?: string
}

const SEEDS: Seed[] = [
  {
    label: 'Anthropic', keyEnv: 'ANTHROPIC_API_KEY', hint: 'sk-ant-…', usedBy: 'Claude Code',
    launchBaseUrlEnv: 'ANTHROPIC_BASE_URL', launchModelEnv: 'ANTHROPIC_MODEL',
  },
  // Codex keeps its endpoint in config.toml, so no launch env var.
  { label: 'OpenAI', keyEnv: 'OPENAI_API_KEY', hint: 'sk-…', usedBy: 'Codex' },
  {
    label: 'xAI', keyEnv: 'GROK_API_KEY', hint: 'xai-…', usedBy: 'Grok',
    launchBaseUrlEnv: 'GROK_BASE_URL',
    defaultBase: 'https://api.x.ai', modelHint: 'grok-4',
  },
  {
    label: 'Amp (Sourcegraph)', keyEnv: 'AMP_API_KEY', hint: 'sgamp-…', usedBy: 'Amp',
    note: 'Amp reads its endpoint from its settings file, not an environment variable — set it in the launch form’s config editor. Anything else can go in Custom secrets below.',
  },
]

export const PROVIDER_SETTINGS: ProviderSetting[] = SEEDS.map(seed => {
  const configure = CONFIGURE_BASE_URLS[seed.keyEnv]
  const launchBaseUrlEnv = seed.launchBaseUrlEnv || undefined
  const launchModelEnv = seed.launchModelEnv || undefined

  // One field if either consumer reads a base URL; when both do it is the same variable.
  const baseUrlEnv = configure?.baseUrlEnv ?? launchBaseUrlEnv
  const modelEnv = configure?.modelEnv ?? launchModelEnv

  const baseUrlUse: BaseUrlUse[] = []
  if (configure) baseUrlUse.push('configure')
  if (launchBaseUrlEnv) baseUrlUse.push('launch')

  return {
    label: seed.label,
    keyEnv: seed.keyEnv,
    hint: seed.hint,
    usedBy: seed.usedBy,
    baseUrlEnv,
    modelEnv,
    defaultBase: configure?.defaultBase ?? seed.defaultBase,
    modelHint: configure?.modelHint ?? seed.modelHint,
    baseUrlUse: baseUrlUse.length ? baseUrlUse : undefined,
    note: baseUrlEnv ? undefined : seed.note,
  }
})

/** managedVaultNames lists every vault name the provider rows own, so they are not also shown as custom. */
export function managedVaultNames(): Set<string> {
  const names = new Set<string>()
  for (const p of PROVIDER_SETTINGS) {
    names.add(p.keyEnv)
    if (p.baseUrlEnv) names.add(p.baseUrlEnv)
    if (p.modelEnv) names.add(p.modelEnv)
  }
  return names
}

/** TypedOverrides says which fields the user filled in on the launch form. */
export interface TypedOverrides {
  key: boolean
  baseUrl: boolean
  model: boolean
}

/**
 * launchSecretRefs returns the vault names a launch should reference. A typed
 * value suppresses its ref: the server appends resolved refs after `env`, so
 * referencing it too would override what the user typed.
 */
export function launchSecretRefs(
  agent: { keyEnv: string; baseUrlEnv: string; modelEnv: string } | null,
  vaultNames: string[],
  typed: TypedOverrides,
): string[] {
  if (!agent) return []
  const has = (n?: string) => !!n && vaultNames.includes(n)
  const refs: string[] = []
  if (!typed.key && has(agent.keyEnv)) refs.push(agent.keyEnv)
  // Only env vars this agent's CLI reads.
  if (!typed.baseUrl && has(agent.baseUrlEnv)) refs.push(agent.baseUrlEnv)
  if (!typed.model && has(agent.modelEnv)) refs.push(agent.modelEnv)
  return refs
}

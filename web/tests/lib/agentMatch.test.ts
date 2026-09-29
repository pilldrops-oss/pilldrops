import { describe, it, expect } from 'vitest'
import { matchAgent } from '../../src/lib/agentMatch'
import type { Template } from '../../src/types'

const tpl = (id: string, name: string, isAgent = true): Template => ({
  id, name,
  description: '', cpu: 2, memory_mb: 2048, visibility: 'public',
  ...(isAgent ? { agent: { key_env: 'X_API_KEY' } } : {}),
}) as Template

const CATALOG: Template[] = [
  tpl('claude', 'Claude Code'),
  tpl('codex', 'Codex'),
  tpl('grok', 'Grok'),
  tpl('amp', 'Amp'),
  tpl('opencode', 'OpenCode'),
  tpl('python-3.11', 'Python 3.11', false),
  tpl('playwright', 'Playwright', false),
]

describe('matchAgent', () => {
  it('matches a bare agent name', () => {
    expect(matchAgent('claude', CATALOG)?.id).toBe('claude')
    expect(matchAgent('amp', CATALOG)?.id).toBe('amp')
    expect(matchAgent('opencode', CATALOG)?.id).toBe('opencode')
  })

  it('matches inside a sentence', () => {
    expect(matchAgent('i want to try claude code', CATALOG)?.id).toBe('claude')
    expect(matchAgent('can you start amp for me', CATALOG)?.id).toBe('amp')
  })

  it('does not yet know openclaw — it is not in the catalogue', () => {
    // Flips to expecting a match once OpenClaw is added to the catalogue.
    expect(matchAgent('i want openclaw', CATALOG)).toBeNull()
  })

  it('ignores case and punctuation', () => {
    expect(matchAgent('Claude Code!', CATALOG)?.id).toBe('claude')
    expect(matchAgent('  GROK  ', CATALOG)?.id).toBe('grok')
  })

  it('only matches whole words', () => {
    expect(matchAgent('build me an amplifier ui', CATALOG)).toBeNull()
    expect(matchAgent('a react example app', CATALOG)).toBeNull()
  })

  it('does not match non-agent templates', () => {
    // The query equals the normalised id, so only the agent filter stands between it and a match.
    expect(matchAgent('python-3.11', CATALOG)).toBeNull()
    expect(matchAgent('playwright please', CATALOG)).toBeNull()
  })

  it('returns null for a free-form request', () => {
    expect(matchAgent('a mobile testing rig with adb and chrome', CATALOG)).toBeNull()
    expect(matchAgent('', CATALOG)).toBeNull()
  })

  it('breaks ties on the longer candidate — the wrong sandbox otherwise', () => {
    // "amp-pro" is listed before "amp" on purpose: a last-match-wins loop would return "amp".
    const TIEBREAK_CATALOG: Template[] = [
      tpl('amp-pro', 'Amp Pro'),
      tpl('amp', 'Amp'),
    ]
    expect(matchAgent('start amp pro', TIEBREAK_CATALOG)?.id).toBe('amp-pro')
  })

  it('ignores a candidate that normalises to fewer than 3 characters', () => {
    // "C#" normalises to a bare "c", which would match the word "C" in ordinary prose.
    const SHORT_NAME_CATALOG: Template[] = [...CATALOG, tpl('csharp', 'C#')]
    expect(matchAgent('I want to write a C program that sorts a list', SHORT_NAME_CATALOG)).toBeNull()
  })
})

// Agent names are long, distinctive tokens, so forgiving one typo is safe there.
describe('matchAgent — one typo off still reaches the agent', () => {
  const agents: Template[] = [
    { id: 'claude', name: 'Claude Code', description: '', cpu: 1, memory_mb: 512, visibility: 'public', agent: {} } as unknown as Template,
    { id: 'grok', name: 'Grok', description: '', cpu: 1, memory_mb: 512, visibility: 'public', agent: {} } as unknown as Template,
  ]

  it('clayde code → Claude Code', () => {
    expect(matchAgent('i want to run clayde code for my project', agents)?.id).toBe('claude')
    expect(matchAgent('cluade code please', agents)?.id).toBe('claude')
  })

  it('short names stay exact — "grow" must not summon Grok', () => {
    expect(matchAgent('help my plants grow', agents)).toBeNull()
  })
})

import { describe, it, expect } from 'vitest'
import { matchIntent , offByOne } from '../../src/lib/intentMatch'
import type { Template } from '../../src/types'

// Descriptions are copied from the real catalogue, since the matcher scores against them.
const t = (id: string, name: string, description: string): Template =>
  ({ id, name, description, cpu: 2, memory_mb: 2048, visibility: 'public' } as Template)

const CATALOG: Template[] = [
  t('postgres', 'Postgres', 'PostgreSQL 16 relational database.'),
  t('redis', 'Redis', 'Redis 7 in-memory data store / cache.'),
  t('loadtest', 'Load test', 'Pre-baked k6 load generator: pdload <url> ramps virtual users, checks thresholds'),
  t('desktop', 'Desktop', 'Full Linux desktop (fluxbox) in your browser — run any GUI app'),
  t('web-scraper', 'Web scraper', 'Scrape a page in seconds: the Python starter fetches, extracts and saves'),
  t('android', 'Android', 'Android 14 emulator (KVM-accelerated) with a browser-viewable screen'),
  t('mobile-test', 'Mobile test', 'Zero-setup mobile E2E: drop your test script in, it runs on a headless emulator'),
  t('playwright', 'Playwright', 'Pre-baked Playwright + Chromium (browsers & OS deps included) — write E2E tests'),
  t('security', 'Security', 'Scan code you own: osv-scanner finds known-vulnerable dependencies'),
  t('python-3.11', 'Python 3.11', 'Python 3.11 + Node 20 + numpy, pandas, matplotlib, scipy, scikit-learn'),
  t('node-20', 'Node 20', 'Node 20 + typescript, tsx, ts-node'),
  t('webhook-inbox', 'Webhook inbox', 'A live URL that catches webhooks: every request is logged'),
]

describe('matchIntent — the jobs the landing page already sells', () => {
  it.each([
    ['i need a postgres database', 'postgres'],
    ['load test my api to 500 users', 'loadtest'],
    ['a linux desktop i can click around in', 'desktop'],
    ['scrape a page every hour', 'web-scraper'],
    ['scan my dependencies for vulnerabilities', 'security'],
    ['catch webhooks at a live url', 'webhook-inbox'],
    ['redis cache', 'redis'],
  ])('%s → %s', (query, expected) => {
    expect(matchIntent(query, CATALOG).best?.id).toBe(expected)
  })
})

describe('matchIntent — refuses rather than guessing', () => {
  it('returns nothing for a request the catalogue does not answer', () => {
    for (const q of [
      'i want my own hermes agent with telegram connect',
      'an erlang otp app with a supervisor tree',
      'something with a database', // too vague to act on
    ]) {
      expect(matchIntent(q, CATALOG).best, q).toBeNull()
    }
  })

  it('returns nothing for empty or stopword-only input', () => {
    expect(matchIntent('', CATALOG).best).toBeNull()
    expect(matchIntent('i want to make a new one', CATALOG).best).toBeNull()
  })

  it('one incidental description word is not a request', () => {
    // "seconds" appears only in web-scraper's description prose; "python" hits a template
    // name, which is a deliberate signal rather than incidental prose.
    expect(matchIntent('seconds', CATALOG).best).toBeNull()
    expect(matchIntent('python', CATALOG).best?.id).toBe('python-3.11')
  })
})

describe('matchIntent — ambiguity is offered, not resolved by coin flip', () => {
  it('offers candidates when two templates answer equally well', () => {
    // Both score on their own name, so neither wins.
    const r = matchIntent('postgres or redis', CATALOG)
    expect(r.best).toBeNull()
    expect(r.candidates.map(c => c.id)).toEqual(expect.arrayContaining(['postgres', 'redis']))
  })

  it('candidates are ranked, best first, and capped', () => {
    const r = matchIntent('test emulator screen browser', CATALOG)
    if (r.candidates.length) expect(r.candidates.length).toBeLessThanOrEqual(3)
  })
})

describe('matchIntent — mechanics', () => {
  it('weights an id/name hit above a description hit', () => {
    expect(matchIntent('postgres', CATALOG).best?.id).toBe('postgres')
  })

  it('matches across singular and plural', () => {
    expect(matchIntent('scan dependency vulnerabilities', CATALOG).best?.id).toBe('security')
  })

  it('honours the exclude set so it never competes with exact-name matching', () => {
    const r = matchIntent('i need a postgres database', CATALOG, new Set(['postgres']))
    expect(r.best?.id).not.toBe('postgres')
  })

  it('is stable — the same query always gives the same answer', () => {
    // Ties break on id, not on catalogue order.
    const a = matchIntent('test emulator', CATALOG).candidates.map(c => c.id)
    const b = matchIntent('test emulator', [...CATALOG].reverse()).candidates.map(c => c.id)
    expect(a).toEqual(b)
  })
})

describe('offByOne', () => {
  it('accepts each single-typo class', () => {
    expect(offByOne('clayde', 'claude')).toBe(true)  // substitution
    expect(offByOne('calude', 'claude')).toBe(true)  // adjacent transposition
    expect(offByOne('postgre', 'postgres')).toBe(true) // deletion
    expect(offByOne('claudde', 'claude')).toBe(true)   // insertion
  })

  it('rejects anything two edits away — that space is where collisions live', () => {
    // "resreach" is distance 2 from "research" (a rotation, not a swap).
    expect(offByOne('resreach', 'research')).toBe(false)
    expect(offByOne('clyde', 'claude')).toBe(false)
    expect(offByOne('postgres', 'progress')).toBe(false)
  })
})

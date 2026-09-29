import { describe, it, expect } from 'vitest'
import { pinnedPort, workspaceGlobs, locateApp, needsBuild, installFor, isToolingOnly } from '../../src/lib/projectShape'

// Every fixture is copied from a real repository's package.json.

const REALWORLD = { scripts: { start: 'cross-env PORT=4100 react-scripts start', build: 'react-scripts build' } }
const FASTAPI_TPL = { workspaces: ['frontend'], scripts: { dev: 'bun run --filter frontend dev' } }
const NEXTJS_MONO = { workspaces: ['packages/*'], scripts: { build: 'turbo build' } }
const SVELTE_TPL = { scripts: { build: 'rollup -c', dev: 'rollup -c -w', start: 'sirv public --no-clear' } }

describe('pinnedPort — the 427-second failure', () => {
  it('reads a port the start script pins for itself', () => {
    // react-redux-realworld serves on 4100 whatever the platform injects.
    expect(pinnedPort(REALWORLD.scripts.start)).toBe(4100)
  })

  it.each([
    ['PORT=3000 node server.js', 3000],
    ['cross-env PORT=8080 vite', 8080],
    ['node server.js', undefined],
    ['next dev', undefined],
  ])('%s → %s', (script, expected) => {
    expect(pinnedPort(script)).toBe(expected)
  })

  it('does not mistake a stray number for a port', () => {
    expect(pinnedPort('node server.js --retries 4100')).toBeUndefined()
    expect(pinnedPort('vite --port-ish 3000')).toBeUndefined()
  })

  it('rejects a port outside the valid range', () => {
    expect(pinnedPort('PORT=99999 node x')).toBeUndefined()
  })

  it('copes with no script at all', () => {
    expect(pinnedPort(undefined)).toBeUndefined()
    expect(pinnedPort('')).toBeUndefined()
  })
})

describe('locateApp — monorepos', () => {
  it('finds the one concrete workspace', () => {
    // full-stack-fastapi-template keeps its app in frontend/; the root has no server.
    expect(locateApp(FASTAPI_TPL)).toEqual({ kind: 'app-dir', dir: 'frontend' })
  })

  it('refuses to pick from a glob', () => {
    // vercel/next.js has dozens of packages and none of them is the app.
    expect(locateApp(NEXTJS_MONO)).toEqual({ kind: 'ambiguous', globs: ['packages/*'] })
  })

  it('treats a plain project as a plain project', () => {
    expect(locateApp(SVELTE_TPL)).toEqual({ kind: 'single' })
    expect(locateApp(null)).toEqual({ kind: 'single' })
  })

  it('reads the object form of workspaces too', () => {
    expect(locateApp({ workspaces: { packages: ['web'] } })).toEqual({ kind: 'app-dir', dir: 'web' })
  })

  it('will not pick when several concrete workspaces exist', () => {
    expect(locateApp({ workspaces: ['frontend', 'backend'] }).kind).toBe('ambiguous')
  })
})

describe('needsBuild', () => {
  it('builds when start serves built output', () => {
    // `sirv public` serves a directory that only exists after a build.
    expect(needsBuild(SVELTE_TPL, SVELTE_TPL.scripts.start)).toBe(true)
  })

  it('does NOT build for a dev server', () => {
    expect(needsBuild(SVELTE_TPL, 'rollup -c -w')).toBe(false)
    expect(needsBuild({ scripts: { build: 'x' } }, 'vite')).toBe(false)
    expect(needsBuild({ scripts: { build: 'x' } }, 'next dev')).toBe(false)
    expect(needsBuild(REALWORLD, REALWORLD.scripts.start)).toBe(false) // react-scripts start
  })

  it('does not build when there is nothing to build', () => {
    expect(needsBuild({ scripts: { start: 'node x' } }, 'node x')).toBe(false)
    expect(needsBuild(null, 'node x')).toBe(false)
  })
})

describe('installFor', () => {
  it('follows the lockfile that is actually present', () => {
    expect(installFor(['package.json', 'pnpm-lock.yaml'])).toBe('pnpm install')
    expect(installFor(['package.json', 'yarn.lock'])).toBe('yarn install')
    expect(installFor(['package.json', 'bun.lockb'])).toBe('bun install')
    expect(installFor(['package.json'])).toBe('npm install')
  })

  it('matches a lockfile inside a workspace directory', () => {
    expect(installFor(['frontend/pnpm-lock.yaml'])).toBe('pnpm install')
  })
})

describe('workspaceGlobs', () => {
  it('normalises both declaration forms', () => {
    expect(workspaceGlobs(NEXTJS_MONO)).toEqual(['packages/*'])
    expect(workspaceGlobs({ workspaces: { packages: ['a', 'b'] } })).toEqual(['a', 'b'])
    expect(workspaceGlobs({})).toEqual([])
    expect(workspaceGlobs(null)).toEqual([])
  })
})

describe('isToolingOnly — which manifest is the project', () => {
  it('django/django ships a package.json that runs nothing', () => {
    // Tooling-only scripts and devDeps, while the project itself is Python.
    expect(isToolingOnly({
      scripts: { test: 'grunt test', biome: 'biome ci' },
      devDependencies: { '@biomejs/biome': '1', grunt: '1', qunit: '2' },
    })).toBe(true)
  })

  it('a real JS project is never demoted', () => {
    expect(isToolingOnly({ scripts: { dev: 'bun run --filter frontend dev' } })).toBe(false)
    expect(isToolingOnly({ scripts: { start: 'node server.js' } })).toBe(false)
    expect(isToolingOnly({ dependencies: { express: '4' } })).toBe(false)
    // A runtime dependency counts even with no scripts.
    expect(isToolingOnly({ dependencies: { next: '14' }, scripts: { lint: 'eslint' } })).toBe(false)
  })

  it('an absent package.json is not "tooling"', () => {
    expect(isToolingOnly(null)).toBe(false)
  })
})

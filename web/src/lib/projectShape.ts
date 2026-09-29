// Facts read from package.json that detectProject alone gets wrong: pinned
// ports, monorepo workspaces, and start scripts that need a build first.

export interface PackageJson {
  scripts?: Record<string, string>
  workspaces?: string[] | { packages?: string[] }
  main?: string
  dependencies?: Record<string, string>
  devDependencies?: Record<string, string>
}

/**
 * isToolingOnly reports whether package.json is just JS tooling (lint, docs) in
 * a non-Node project: no runtime deps and no start/dev/serve script.
 */
export function isToolingOnly(pkg: PackageJson | null): boolean {
  if (!pkg) return false
  const runtimeDeps = Object.keys(pkg.dependencies ?? {}).length > 0
  const s = pkg.scripts ?? {}
  const runnable = !!(s.start || s.dev || s.serve)
  return !runtimeDeps && !runnable
}

/**
 * pinnedPort returns the port a start script assigns itself (`PORT=4100 ...`),
 * which overrides whatever the platform injects. Only explicit assignments count.
 */
export function pinnedPort(script: string | undefined): number | undefined {
  if (!script) return undefined
  const m = script.match(/(?:^|\s)(?:PORT|port)=(\d{2,5})(?:\s|$)/)
  if (!m) return undefined
  const n = Number(m[1])
  return n >= 1 && n <= 65535 ? n : undefined
}

/** workspaceGlobs returns the root package.json workspace globs, normalised. */
export function workspaceGlobs(pkg: PackageJson | null): string[] {
  if (!pkg?.workspaces) return []
  return Array.isArray(pkg.workspaces) ? pkg.workspaces : (pkg.workspaces.packages ?? [])
}

export type MonorepoVerdict =
  /** Not a monorepo; the root is the project. */
  | { kind: 'single' }
  /** One concrete workspace directory to look in. */
  | { kind: 'app-dir'; dir: string }
  /** A monorepo with no single app to pick; ask rather than guess. */
  | { kind: 'ambiguous'; globs: string[] }

/**
 * locateApp says where the runnable app lives. A single concrete workspace is
 * safe; a glob like "packages/*" is ambiguous and must not be guessed at.
 */
export function locateApp(pkg: PackageJson | null): MonorepoVerdict {
  const globs = workspaceGlobs(pkg)
  if (globs.length === 0) return { kind: 'single' }
  const concrete = globs.filter(g => !g.includes('*'))
  if (concrete.length === 1) return { kind: 'app-dir', dir: concrete[0] }
  return { kind: 'ambiguous', globs }
}

/**
 * needsBuild reports whether the start command serves built output (`sirv public`)
 * rather than compiling on the fly. Judged from the command, not the script name.
 */
export function needsBuild(pkg: PackageJson | null, startScript: string | undefined): boolean {
  if (!pkg?.scripts?.build || !startScript) return false
  const isDevServer =
    /(^|\s)(vite|nodemon|next\s+dev|react-scripts\s+start|rollup\s+-c\s+-w|webpack\s+serve|--watch)(\s|$)/.test(
      startScript,
    )
  return !isDevServer
}

/** installFor returns the install command implied by the lockfiles present. */
export function installFor(paths: string[]): string {
  if (paths.some(p => p.endsWith('pnpm-lock.yaml'))) return 'pnpm install'
  if (paths.some(p => p.endsWith('yarn.lock'))) return 'yarn install'
  if (paths.some(p => p.endsWith('bun.lockb'))) return 'bun install'
  return 'npm install'
}

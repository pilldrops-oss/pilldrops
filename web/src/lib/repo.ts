// Turns a GitHub repo URL into a clone command and probed manifests. Rule-based,
// no model call, shared by Deploy and Build.

/** GitHubRepo is a parsed repo; only parseGitHub produces one. */
export interface GitHubRepo {
  owner: string
  repo: string
}

/**
 * parseGitHub accepts a full URL (scheme, .git and trailing slash optional) or
 * bare `owner/repo`. The character class is the security boundary: both fields
 * end up in a shell command, so anything else must fail here, not be quoted later.
 */
export function parseGitHub(u: string): GitHubRepo | null {
  const s = u.trim()
  const m =
    s.match(/^(?:https?:\/\/)?(?:www\.)?github\.com\/([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+?)(?:\.git)?\/?$/) ??
    s.match(/^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+?)(?:\.git)?$/)
  if (!m) return null
  // "." and ".." pass the class above but are not repos.
  if (/^\.+$/.test(m[1]) || /^\.+$/.test(m[2])) return null
  return { owner: m[1], repo: m[2] }
}

/** safeRef returns a ref safe to interpolate into a command, or 'HEAD'. */
export function safeRef(ref: string): string {
  const r = ref.trim()
  return r && /^[\w./-]+$/.test(r) ? r : 'HEAD'
}

/**
 * CLONE_DIR is where a cloned repo lands. Not "app": some images bake a starter
 * there, the clone fails, and the sandbox silently serves the template's app instead.
 */
export const CLONE_DIR = 'pd-repo'

/** cloneCommand builds the clone from parsed parts only; the GitHubRepo type is the validation guarantee. */
export function cloneCommand(gh: GitHubRepo, ref = 'HEAD', dir = CLONE_DIR): string {
  const r = safeRef(ref)
  const branch = r === 'HEAD' ? '' : ` --branch ${r}`
  return `git clone --depth 1${branch} https://github.com/${gh.owner}/${gh.repo}.git ${dir}`
}

/** MANIFESTS lists the files worth probing, most explicit first. */
export const MANIFESTS = [
  'pilldrops.json',
  'package.json',
  'playwright.config.ts',
  'playwright.config.js',
  'next.config.js',
  'requirements.txt',
  'pyproject.toml',
  'go.mod',
  'Cargo.toml',
  'Dockerfile',
  'pnpm-lock.yaml',
  'yarn.lock',
  // Entry points, so a start command is checked against a file that exists.
  'main.py',
  'app.py',
  'wsgi.py',
  'asgi.py',
  'manage.py',
  'server.js',
  'index.js',
] as const

export interface ProbedFile {
  path: string
  content: string
}

const SUB_MANIFESTS = ['package.json', 'pnpm-lock.yaml', 'yarn.lock', 'bun.lockb', 'next.config.js'] as const

/** probeWorkspace reads a workspace subdirectory's manifests; paths come back prefixed with the directory. */
export async function probeWorkspace(
  fetchImpl: typeof fetch,
  gh: GitHubRepo,
  dir: string,
  ref = 'HEAD',
): Promise<ProbedFile[]> {
  const r = safeRef(ref)
  // dir came from a fetched package.json and ends up in a URL; refuse rather than escape.
  if (!/^[\w.-]+(?:\/[\w.-]+)*$/.test(dir)) return []
  const found: ProbedFile[] = []
  await Promise.all(
    SUB_MANIFESTS.map(async f => {
      try {
        const res = await fetchImpl(
          `https://raw.githubusercontent.com/${gh.owner}/${gh.repo}/${r}/${dir}/${f}`,
          { cache: 'no-store' },
        )
        if (res.ok) found.push({ path: `${dir}/${f}`, content: (await res.text()).slice(0, 100_000) })
      } catch {
        /* absent */
      }
    }),
  )
  return found
}

/**
 * probeRepoManifests reads a public repo's manifests from raw.githubusercontent.com.
 * A 404, private repo or network error is just "absent"; the caller must ask, not assume.
 */
export async function probeRepoManifests(
  fetchImpl: typeof fetch,
  gh: GitHubRepo,
  ref = 'HEAD',
): Promise<ProbedFile[]> {
  const r = safeRef(ref)
  const found: ProbedFile[] = []
  await Promise.all(
    MANIFESTS.map(async f => {
      try {
        const res = await fetchImpl(
          `https://raw.githubusercontent.com/${gh.owner}/${gh.repo}/${r}/${f}`,
          { cache: 'no-store' },
        )
        if (res.ok) found.push({ path: f, content: (await res.text()).slice(0, 100_000) })
      } catch {
        /* absent */
      }
    }),
  )
  return found
}

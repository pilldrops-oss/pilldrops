import { isToolingOnly, type PackageJson } from './projectShape'

// Client-side sniffing of an uploaded project. Output only pre-fills the form; the user can override everything.

function nodeLabel(deps: Record<string, unknown>): string {
  if (deps.express) return 'Express app'
  if (deps.fastify) return 'Fastify app'
  return 'Node project'
}

export interface Detection {
  /** Suggested template id; undefined means no switch suggested. */
  templateId?: string
  label: string
  start?: string
  setup?: string
  port?: number
}

interface ProjFile {
  path: string
  content: string
}

// Top-level files are preferred.
function find(files: ProjFile[], re: RegExp): ProjFile | undefined {
  const base = (p: string) => p.split('/').pop() ?? p
  return (
    files.find(f => !f.path.includes('/') && re.test(base(f.path))) ??
    files.find(f => re.test(base(f.path)))
  )
}

function parseJSON<T>(raw: string): T | null {
  try {
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

// detectProject checks the most specific signals first.
export function detectProject(files: ProjFile[]): Detection | null {
  if (!files.length) return null

  if (find(files, /^playwright\.config\.(js|ts|mjs|cjs)$/)) {
    return {
      templateId: 'playwright',
      label: 'Playwright project',
      start: 'npx playwright test',
      setup: find(files, /^package\.json$/) ? npmInstallFor(files) : undefined,
    }
  }
  if (files.length === 1 && /\.spec\.(js|ts|mjs)$/.test(files[0].path)) {
    return { templateId: 'playwright', label: 'Playwright spec', start: 'npx playwright test' }
  }

  const pkgFile = find(files, /^package\.json$/)
  // A tooling-only package.json beside another language's manifest (Django's lint
  // scripts next to pyproject.toml) is not a Node project.
  const otherLang = find(files, /^(requirements\.txt|pyproject\.toml|go\.mod|Cargo\.toml)$/)
  if (pkgFile && !(otherLang && isToolingOnly(parseJSON<PackageJson>(pkgFile.content)))) {
    const pkg = parseJSON<{
      main?: string
      scripts?: Record<string, string>
      dependencies?: Record<string, string>
      devDependencies?: Record<string, string>
    }>(pkgFile.content)
    const deps = { ...(pkg?.dependencies ?? {}), ...(pkg?.devDependencies ?? {}) }
    const install = npmInstallFor(files)
    if (deps.next || find(files, /^next\.config\.(js|ts|mjs)$/)) {
      return {
        templateId: 'node',
        label: 'Next.js project',
        start: runScript(files, pkg?.scripts, ['dev', 'start']) ?? 'npm run dev',
        setup: install,
      }
    }
    const start =
      runScript(files, pkg?.scripts, ['start', 'dev']) ??
      (pkg?.main ? `node ${pkg.main}` : undefined)
    return {
      templateId: 'node',
      label: nodeLabel(deps),
      start,
      setup: install,
    }
  }

  const reqs = find(files, /^requirements\.txt$/)
  const pyproject = find(files, /^pyproject\.toml$/)
  const pyDeps = (reqs?.content ?? '') + (pyproject?.content ?? '')
  const pyFiles = files.filter(f => f.path.endsWith('.py'))
  if (/fastapi/i.test(pyDeps) || pyFiles.some(f => /FastAPI\s*\(/.test(f.content))) {
    // No app file, no start command: never invent a `main:app` that does not exist.
    const appFile = pyFiles.find(f => /FastAPI\s*\(/.test(f.content))
    return {
      templateId: 'code-interpreter',
      label: appFile ? 'FastAPI app' : 'Python project using FastAPI',
      start: appFile
        ? `uvicorn ${appFile.path.replace(/\.py$/, '').replace(/\//g, '.')}:app --host 0.0.0.0 --port $PORT`
        : undefined,
      setup: reqs ? 'pip install -r requirements.txt' : undefined,
    }
  }
  if (/(^|\n)\s*flask/i.test(pyDeps) || pyFiles.some(f => /Flask\s*\(/.test(f.content))) {
    const appFile = pyFiles.find(f => /Flask\s*\(/.test(f.content)) ?? pyFiles[0]
    return {
      label: 'Flask app',
      start: appFile ? `python3 ${appFile.path}` : undefined,
      setup: reqs ? 'pip install -r requirements.txt' : 'pip install flask',
    }
  }
  if (find(files, /\.ipynb$/) || find(files, /\.(csv|parquet)$/)) {
    return { templateId: 'code-interpreter', label: 'data / notebook files' }
  }
  if (reqs || pyproject || pyFiles.length > 0) {
    return {
      templateId: 'code-interpreter',
      label: 'Python project',
      setup: reqs ? 'pip install -r requirements.txt' : undefined,
    }
  }

  if (find(files, /^go\.mod$/)) {
    return { templateId: 'go', label: 'Go module', start: 'go run .' }
  }
  if (find(files, /^Cargo\.toml$/)) {
    return { templateId: 'rust', label: 'Cargo project', start: 'cargo run' }
  }

  // Dockerfile is note-only; custom images go through the image API.
  const docker = find(files, /^Dockerfile$/)
  if (docker) {
    const expose = docker.content.match(/^EXPOSE\s+(\d+)/m)
    return {
      label: 'Dockerfile',
      port: expose ? parseInt(expose[1], 10) : undefined,
    }
  }

  return null
}

function runScript(
  files: ProjFile[],
  scripts: Record<string, string> | undefined,
  order: string[],
): string | undefined {
  if (!scripts) return undefined
  const pm = pmFor(files)
  for (const name of order) {
    if (scripts[name]) return `${pm} run ${name}`
  }
  return undefined
}

function pmFor(files: ProjFile[]): string {
  if (find(files, /^pnpm-lock\.yaml$/)) return 'pnpm'
  if (find(files, /^yarn\.lock$/)) return 'yarn'
  return 'npm'
}

function npmInstallFor(files: ProjFile[]): string {
  const pm = pmFor(files)
  return pm === 'yarn' ? 'yarn install' : `${pm} install`
}

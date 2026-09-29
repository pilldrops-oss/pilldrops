import type { Template } from '../types'

// Starter file derived from the template's start command, so `uvicorn main:app`
// on an image with no main.py resolves by construction.

export type ScaffoldLang = 'python' | 'node'

export interface Entrypoint {
  /** Path relative to the sandbox working directory. */
  path: string
  lang: ScaffoldLang
}

/**
 * entrypointFor returns the file a start command will look for, or null when
 * unsure (path-qualified commands run existing code; npm scripts resolve in package.json).
 */
export function entrypointFor(start: string | undefined): Entrypoint | null {
  const cmd = (start ?? '').trim()
  if (!cmd) return null

  // cd or an absolute path means the code is already on disk.
  if (/(^|\s)cd\s/.test(cmd) || /\s\//.test(cmd)) return null

  if (/\b(npm|yarn|pnpm)\b/.test(cmd)) return null

  const asgi = cmd.match(/\b(?:uvicorn|gunicorn|hypercorn)\s+([A-Za-z_][\w]*):/)
  if (asgi) return { path: `${asgi[1]}.py`, lang: 'python' }

  const py = cmd.match(/\bpython3?\b[^|&]*?\s([\w.-]+\.py)\b/)
  if (py) return { path: py[1], lang: 'python' }

  const node = cmd.match(/\bnode\b[^|&]*?\s([\w.-]+\.[cm]?js)\b/)
  if (node) return { path: node[1], lang: 'node' }

  return null
}

function reported(env: Record<string, string>): string[] {
  return Object.keys(env).filter(k => k.endsWith('_URL')).sort()
}

function pythonScaffold(vars: string[]): string {
  const lines = vars.map(v => `    "${v}": os.environ.get("${v}", "not set"),`).join('\n')
  return `# Starter app, written by PillDrops so this sandbox serves something.
# Replace it with your own code — or paste a repo and it will be used instead.
import os
from fastapi import FastAPI

app = FastAPI()


@app.get("/")
def root():
    return {
        "ok": True,
        "message": "Your sandbox is serving.",
${lines ? `${lines  }\n` : ''}    }


# Also runnable directly (\`python main.py\`). $PORT is injected by the
# platform; binding 0.0.0.0 rather than the loopback interface is what makes
# the preview URL reachable from outside the sandbox.
if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", 8000)))
`
}

function nodeScaffold(vars: string[]): string {
  const lines = vars.map(v => `      ${v}: process.env.${v} ?? 'not set',`).join('\n')
  return `// Starter app, written by PillDrops so this sandbox serves something.
// Replace it with your own code — or paste a repo and it will be used instead.
const http = require('http')

// $PORT is injected by the platform. Binding 0.0.0.0 rather than the loopback
// interface is what makes the preview URL reachable from outside the sandbox.
const port = process.env.PORT || 8000

http
  .createServer((_req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({
      ok: true,
      message: 'Your sandbox is serving.',
${lines ? `${lines  }\n` : ''}    }, null, 2))
  })
  .listen(port, '0.0.0.0', () => console.log('listening on 0.0.0.0:' + port))
`
}

/** scaffoldFor returns the starter file for a template, echoing the injected `env`, or null when none is needed. */
export function scaffoldFor(t: Template, env: Record<string, string>): { path: string; content: string } | null {
  const entry = entrypointFor(t.start_suggestion)
  if (!entry) return null
  const vars = reported(env)
  return {
    path: entry.path,
    content: entry.lang === 'python' ? pythonScaffold(vars) : nodeScaffold(vars),
  }
}

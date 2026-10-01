import type { Plan, PlannedService } from '../../src/lib/plan'
import type { Template } from '../../src/types'

// Plans arrive from the server. These are the shapes the UI has to handle:
// an app with one store, with two stores, a lone store, a lone agent, and an
// app whose start command names a file its image does not ship.

const t = (o: Partial<Template>): Template =>
  ({ name: o.id, description: '', cpu: 1, memory_mb: 512, visibility: 'public', ...o }) as Template

export const TEMPLATES = {
  postgres: t({ id: 'postgres', name: 'Postgres', kind: 'service', tags: ['database', 'sql', 'postgres'], default_port: 5432 }),
  redis: t({ id: 'redis', name: 'Redis', kind: 'service', tags: ['database', 'cache', 'redis'], default_port: 6379 }),
  // FastAPI has no catalogue id of its own; it resolves to code-interpreter, same as detect.ts.
  fastapi: t({ id: 'code-interpreter', name: 'FastAPI', kind: 'app', tags: ['python', 'api', 'fastapi'], default_port: 8000, start_suggestion: 'uvicorn main:app --port $PORT' }),
  claude: t({ id: 'node', name: 'Claude Code', tags: ['agent'] }),
  weird: t({ id: 'weird', name: 'Weird', kind: 'app', tags: ['weird'], default_port: 8000, start_suggestion: 'node server.js' }),
  // Next.js has no catalogue id of its own either; it resolves to node, same as detect.ts.
  nextjs: t({ id: 'node', name: 'Next.js', kind: 'app', language: 'node', tags: ['nextjs', 'react'], default_port: 8000, start_suggestion: 'cd /root/app && npm run dev' }),
}

const svc = (id: string, role: PlannedService['role'], template: Template, reason: string): PlannedService =>
  ({ id, role, template, reason })

const DB_LINK = { from: 'api', to: 'db', env: 'DATABASE_URL', reason: 'so api can reach Postgres' }
const CACHE_LINK = { from: 'api', to: 'cache', env: 'REDIS_URL', reason: 'so api can reach Redis' }

const db = svc('db', 'database', TEMPLATES.postgres, 'you asked for Postgres')
const cache = svc('cache', 'cache', TEMPLATES.redis, 'you asked for Redis')

export const apiWithDb: Plan = {
  services: [svc('api', 'app', TEMPLATES.fastapi, 'matched on "python"'), db],
  links: [DB_LINK],
  gaps: [],
  confidence: 'derived',
}

export const apiWithDbAndCache: Plan = {
  services: [svc('api', 'app', TEMPLATES.fastapi, 'matched on "python", "api"'), db, cache],
  links: [DB_LINK, CACHE_LINK],
  gaps: [],
  confidence: 'derived',
}

export const weirdWithDb: Plan = {
  services: [svc('api', 'app', TEMPLATES.weird, 'you asked for Weird'), db],
  links: [DB_LINK],
  gaps: [],
  confidence: 'derived',
}

export const nextOnly: Plan = {
  services: [svc('api', 'app', TEMPLATES.nextjs, 'you asked for Next.js')],
  links: [],
  gaps: [],
  confidence: 'derived',
}

export const dbOnly: Plan = { services: [db], links: [], gaps: [], confidence: 'derived' }

export const agentOnly: Plan = {
  services: [svc('agent', 'agent', TEMPLATES.claude, 'you asked for Claude Code by name')],
  links: [],
  gaps: [],
  confidence: 'exact',
}

export const empty: Plan = { services: [], links: [], gaps: [], confidence: 'derived' }

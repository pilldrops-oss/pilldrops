import type { Template } from '../types'
import type { ServiceRole } from './plan'

// Roles derive from the catalogue's `kind` and `tags`, never from template ids, so new
// templates compose without edits here. The link variable name is the one convention the
// manifest does not carry yet.

export function roleOf(t: Template): ServiceRole {
  const tags = (t.tags ?? []).map(s => s.toLowerCase())

  if (t.kind === 'service') {
    // Cache first: Redis tags itself as both 'database' and 'cache'.
    if (tags.includes('cache')) return 'cache'
    if (tags.includes('database') || tags.includes('sql')) return 'database'
    return 'other'
  }
  if (t.kind === 'test' || tags.includes('browser')) return 'browser'
  if (t.kind === 'app' || t.kind === 'code' || t.kind === 'shell') return 'app'
  return 'other'
}

/** isDatastore reports roles something else can be wired to. */
export function isDatastore(role: ServiceRole): boolean {
  return role === 'database' || role === 'cache'
}

const LINK_ENV: Record<string, string> = {
  postgres: 'DATABASE_URL',
  postgresql: 'DATABASE_URL',
  mysql: 'DATABASE_URL',
  mariadb: 'DATABASE_URL',
  mongodb: 'MONGODB_URL',
  redis: 'REDIS_URL',
}

/** linkEnvFor is the variable a consumer of this service receives; unknown datastores get an id-derived name. */
export function linkEnvFor(t: Template): string | null {
  if (!isDatastore(roleOf(t))) return null
  const known = LINK_ENV[t.id.toLowerCase()]
  if (known) return known
  return `${t.id.toUpperCase().replace(/[^A-Z0-9]+/g, '_')}_URL`
}

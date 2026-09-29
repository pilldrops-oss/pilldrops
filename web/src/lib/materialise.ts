import type { Template } from '../types'
import type { Plan, PlannedService } from './plan'
import { isDatastore } from './capability'
import { scaffoldFor, entrypointFor } from './scaffold'

// Datastores are co-hosted inside the app sandbox on 127.0.0.1 rather than run as a
// second sandbox: sandboxes share a flat bridge with no inter-tenant filtering, so a
// separate database sandbox would expose an unauthenticated Postgres to other tenants.

export interface LaunchUnit {
  /** Ids of the logical services this one sandbox carries. */
  hosts: string[]
  template: Template
  setup: string[]
  /** Connection variables, exported onto `start` and written to `.env`. */
  env: Record<string, string>
  files?: Array<{ path: string; content: string }>
  start?: string
  port?: number
}

// Fixed and visible on purpose: the service listens on localhost inside the user's own sandbox.
const PW = 'pilldrops'

interface Colocatable {
  install: string[]
  url: (dbName: string) => string
  envVar: string
}

const COLOCATABLE: Record<string, Colocatable> = {
  postgres: {
    install: [
      'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql',
      'service postgresql start',
      `su postgres -c "psql -tAc \\"ALTER USER postgres PASSWORD '${PW}'\\""`,
      'su postgres -c "createdb app" || true',
    ],
    url: db => `postgresql://postgres:${PW}@127.0.0.1:5432/${db}`,
    envVar: 'DATABASE_URL',
  },
  redis: {
    install: [
      'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq redis-server',
      'service redis-server start',
    ],
    url: () => 'redis://127.0.0.1:6379',
    envVar: 'REDIS_URL',
  },
  mysql: {
    install: [
      'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq default-mysql-server',
      'service mariadb start || service mysql start',
      `mysql -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '${PW}'; CREATE DATABASE IF NOT EXISTS app;" || true`,
    ],
    url: db => `mysql://root:${PW}@127.0.0.1:3306/${db}`,
    envVar: 'DATABASE_URL',
  },
}

// Shell-quotes a value for `export VAR=...`.
function q(v: string): string {
  return `'${v.replace(/'/g, `'\\''`)}'`
}

// True when the start command names a bare entrypoint file that must exist at the root.
function needsEntrypoint(t: Template): boolean {
  return entrypointFor(t.start_suggestion) !== null
}

function unitFor(app: PlannedService, stores: PlannedService[]): LaunchUnit {
  const env: Record<string, string> = {}
  const setup: string[] = []

  for (const s of stores) {
    const c = COLOCATABLE[s.template.id.toLowerCase()]
    // A datastore with no local install recipe is not co-hosted; materialisePlan gives it its own sandbox.
    if (!c) continue
    setup.push(...c.install)
    env[c.envVar] = c.url('app')
  }

  const exports = Object.entries(env)
    .map(([k, v]) => `export ${k}=${q(v)}`)
    .join(' && ')

  // The create API has no env field, so variables ride on the start command. For humans
  // they also go into .bashrc (interactive shell) and profile.d (login shell); a
  // non-interactive exec reads neither, which is why .env is written too.
  if (exports) {
    const lines = Object.entries(env).map(([k, v]) => `export ${k}=${v}`).join('\n')
    setup.push(`printf '%s\\n' ${q(lines)} >> /root/.bashrc`)
    setup.push(`printf '%s\\n' ${q(lines)} > /etc/profile.d/pilldrops-env.sh`)
  }

  const baseStart = app.template.start_suggestion || ''

  // The scaffold is derived from the start command: if it wants main.py, write main.py.
  const scaffold = scaffoldFor(app.template, env)

  const files: Array<{ path: string; content: string }> = []
  if (Object.keys(env).length) {
    files.push({ path: '.env', content: `${Object.entries(env).map(([k, v]) => `${k}=${v}`).join('\n')  }\n` })
  }
  if (scaffold) files.push(scaffold)

  // A start command naming an entrypoint that was neither found nor scaffolded is not sent.
  const canStart = !!baseStart && (!!scaffold || !needsEntrypoint(app.template))
  let start: string | undefined
  if (canStart) start = exports ? `${exports} && ${baseStart}` : baseStart

  return {
    hosts: [app.id, ...stores.filter(s => COLOCATABLE[s.template.id.toLowerCase()]).map(s => s.id)],
    template: app.template,
    setup,
    env,
    files: files.length ? files : undefined,
    start,
    port: app.template.default_port,
  }
}

export function materialisePlan(plan: Plan): LaunchUnit[] {
  const app = plan.services.find(s => s.role === 'app')
  const stores = plan.services.filter(s => isDatastore(s.role))
  const rest = plan.services.filter(s => s !== app && !stores.includes(s))

  const units: LaunchUnit[] = []

  if (app) {
    units.push(unitFor(app, stores))
    // Stores that could not be co-hosted still run as their own sandbox.
    const hosted = new Set(units[0].hosts)
    for (const s of stores.filter(s => !hosted.has(s.id))) {
      units.push({ hosts: [s.id], template: s.template, setup: [], env: {}, port: s.template.default_port })
    }
  } else {
    // No app: each datastore runs from its own catalogue image.
    for (const s of stores) {
      units.push({ hosts: [s.id], template: s.template, setup: [], env: {}, port: s.template.default_port })
    }
  }

  for (const s of rest) {
    units.push({ hosts: [s.id], template: s.template, setup: [], env: {}, port: s.template.default_port })
  }

  return units
}

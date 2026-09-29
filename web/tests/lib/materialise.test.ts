import { describe, it, expect } from 'vitest'
import { materialisePlan } from '../../src/lib/materialise'
import {
  apiWithDb, apiWithDbAndCache, weirdWithDb, nextOnly, dbOnly, agentOnly,
} from './planFixtures'

// Datastores are co-hosted inside the app sandbox because Debian binds them to localhost;
// a separate sandbox would put an unauthenticated database on a network shared with other tenants.
describe('materialisePlan — the start command must be able to succeed', () => {
  // fastapi's start_suggestion is uvicorn main:app, and the image ships no main.py.
  it('writes the file the start command will look for', () => {
    const [u] = materialisePlan(apiWithDb)
    const entry = u.files?.find(f => f.path === 'main.py')
    expect(entry, 'no main.py written for `uvicorn main:app`').toBeTruthy()
  })

  it('makes the scaffold report the variables the plan actually injected', () => {
    const [u] = materialisePlan(apiWithDb)
    expect(u.files?.find(f => f.path === 'main.py')?.content).toContain('DATABASE_URL')
  })

  it('never sends a start command whose entrypoint is missing and unscaffoldable', () => {
    const [u] = materialisePlan(weirdWithDb)
    if (u.start) {
      expect(u.files?.some(f => f.path === 'server.js')).toBe(true)
    }
  })

  it('leaves a template that already ships an app alone', () => {
    const [u] = materialisePlan(nextOnly)
    expect(u.files?.some(f => f.path.endsWith('.js') || f.path.endsWith('.py'))).toBeFalsy()
  })
})

describe('materialisePlan', () => {
  it('co-hosts a datastore inside the app that needs it', () => {
    const units = materialisePlan(apiWithDb)

    expect(units).toHaveLength(1)
    const u = units[0]
    expect(u.template.id).toBe('fastapi')
    expect(u.hosts.sort()).toEqual(['api', 'db'])
  })

  it('installs and starts the datastore in setup', () => {
    const [u] = materialisePlan(apiWithDb)
    const setup = u.setup.join('\n')
    expect(setup).toMatch(/apt-get install .*postgresql/)
    expect(setup).toMatch(/service postgresql start/)
  })

  it('hands the app a connection string that points at localhost', () => {
    const [u] = materialisePlan(apiWithDb)
    expect(u.env.DATABASE_URL).toMatch(/^postgresql:\/\/.*@127\.0\.0\.1:5432\//)
    // Never the bridge IP; localhost is what keeps a neighbouring tenant out.
    expect(u.env.DATABASE_URL).not.toMatch(/192\.168\./)
  })

  it('co-hosts several datastores in one sandbox', () => {
    const [u, ...rest] = materialisePlan(apiWithDbAndCache)
    expect(rest).toHaveLength(0)
    expect(u.env.DATABASE_URL).toContain('127.0.0.1:5432')
    expect(u.env.REDIS_URL).toContain('127.0.0.1:6379')
    expect(u.setup.join('\n')).toMatch(/redis-server/)
  })

  it('runs a lone datastore as its own sandbox, unchanged', () => {
    const units = materialisePlan(dbOnly)
    expect(units).toHaveLength(1)
    expect(units[0].template.id).toBe('postgres')
    expect(units[0].setup).toEqual([])
    expect(units[0].env).toEqual({})
  })

  it('leaves an agent alone', () => {
    const units = materialisePlan(agentOnly)
    expect(units).toHaveLength(1)
    expect(units[0].template.id).toBe('claude')
    expect(units[0].setup).toEqual([])
  })

  it('exports the variables into the start command, since create takes no env', () => {
    const [u] = materialisePlan(apiWithDb)
    expect(u.start).toMatch(/export DATABASE_URL=/)
    expect(u.start).toContain('uvicorn')
  })

  it('writes a .env so the value is visible and frameworks can read it', () => {
    const [u] = materialisePlan(apiWithDb)
    const env = u.files?.find(f => f.path === '.env')
    expect(env?.content).toMatch(/DATABASE_URL=postgresql:\/\//)
  })

  it('never drops a planned service', () => {
    for (const plan of [apiWithDbAndCache, dbOnly, agentOnly]) {
      const hosted = materialisePlan(plan).flatMap(u => u.hosts)
      expect(hosted.sort()).toEqual(plan.services.map(s => s.id).sort())
    }
  })
})

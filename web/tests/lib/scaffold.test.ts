import { describe, it, expect } from 'vitest'
import { entrypointFor, scaffoldFor } from '../../src/lib/scaffold'
import type { Template } from '../../src/types'

const tpl = (o: Partial<Template>): Template =>
  ({ id: 'x', name: 'X', description: '', cpu: 1, memory_mb: 512, visibility: 'public', ...o }) as Template

// The scaffold is derived from the start command so the command resolves by construction.
describe('entrypointFor', () => {
  it('reads the module uvicorn will import', () => {
    expect(entrypointFor('uvicorn main:app --host 0.0.0.0 --port $PORT'))
      .toEqual({ path: 'main.py', lang: 'python' })
  })

  it('reads the file node will run', () => {
    expect(entrypointFor('node server.js')).toEqual({ path: 'server.js', lang: 'node' })
    expect(entrypointFor('node index.js')).toEqual({ path: 'index.js', lang: 'node' })
  })

  it('reads a plain python invocation', () => {
    expect(entrypointFor('python app.py')).toEqual({ path: 'app.py', lang: 'python' })
    expect(entrypointFor('python3 -u main.py')).toEqual({ path: 'main.py', lang: 'python' })
  })

  // A path-qualified command runs an app that is already there, like nextjs's /root/app starter.
  it('leaves a path-qualified command alone', () => {
    expect(entrypointFor('cd /root/app && npm run dev')).toBeNull()
    expect(entrypointFor('node /root/app/server.js')).toBeNull()
  })

  // An npm script's target lives in package.json, which is not available at plan time.
  it('does not guess what an npm script runs', () => {
    expect(entrypointFor('npm run start')).toBeNull()
    expect(entrypointFor('npm start')).toBeNull()
  })

  it('says nothing for an empty or unrecognised command', () => {
    expect(entrypointFor('')).toBeNull()
    expect(entrypointFor(undefined)).toBeNull()
    expect(entrypointFor('/bin/bash')).toBeNull()
  })
})

describe('scaffoldFor', () => {
  const py = tpl({ id: 'fastapi', start_suggestion: 'uvicorn main:app --host 0.0.0.0 --port $PORT' })
  const node = tpl({ id: 'node-20', start_suggestion: 'node server.js' })

  it('writes the exact file the start command will look for', () => {
    expect(scaffoldFor(py, {})?.path).toBe('main.py')
    expect(scaffoldFor(node, {})?.path).toBe('server.js')
  })

  it('binds 0.0.0.0 and the injected $PORT, never localhost', () => {
    for (const t of [py, node]) {
      const c = scaffoldFor(t, {})!.content
      expect(c, t.id).toContain('0.0.0.0')
      expect(c, t.id).toMatch(/PORT/)
      expect(c, t.id).not.toMatch(/127\.0\.0\.1|localhost/)
    }
  })

  it('reports the datastore variables the plan injected', () => {
    const c = scaffoldFor(py, { DATABASE_URL: 'postgresql://…', REDIS_URL: 'redis://…' })!.content
    expect(c).toContain('DATABASE_URL')
    expect(c).toContain('REDIS_URL')
  })

  it('does not mention a variable the plan did not inject', () => {
    const c = scaffoldFor(py, {})!.content
    expect(c).not.toContain('DATABASE_URL')
  })

  it('writes nothing when the start command needs no scaffold', () => {
    expect(scaffoldFor(tpl({ id: 'nextjs', start_suggestion: 'cd /root/app && npm run dev' }), {})).toBeNull()
    expect(scaffoldFor(tpl({ id: 'base' }), {})).toBeNull()
  })

  it('produces syntactically plausible output — balanced braces for node', () => {
    const c = scaffoldFor(node, { DATABASE_URL: 'x' })!.content
    expect(c.split('{').length).toBe(c.split('}').length)
  })
})

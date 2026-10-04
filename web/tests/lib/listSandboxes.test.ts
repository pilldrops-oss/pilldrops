import { describe, it, expect, vi, afterEach } from 'vitest'
import { listSandboxes } from '../../src/api'

// The orchestrator's list names the lifecycle `state`; a node's names it `status`.
// Every page reads `status`, so the one list call must hand back that name.
afterEach(() => vi.unstubAllGlobals())

function serve(body: unknown) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status: 200 })))
}

describe('listSandboxes', () => {
  it('reads the orchestrator state as status', async () => {
    serve({ sandboxes: [{ id: 'a', state: 'running' }, { id: 'b', state: 'paused' }] })
    const { sandboxes } = await listSandboxes()
    expect(sandboxes.map(s => s.status)).toEqual(['running', 'paused'])
  })

  it('keeps a node status as it is', async () => {
    serve({ sandboxes: [{ id: 'a', status: 'paused', state: 'running' }] })
    expect((await listSandboxes()).sandboxes[0].status).toBe('paused')
  })

  it('treats a null list as empty', async () => {
    serve({ sandboxes: null })
    expect((await listSandboxes()).sandboxes).toEqual([])
  })
})

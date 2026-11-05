import { describe, expect, it } from 'vitest'
import { composeCreateBody } from '../../src/lib/launchBody'
import type { Template } from '../../src/types'

// A shell on the preview port is a root shell for anyone who can open the preview,
// and it holds the port a customer's app needs.
describe('composeCreateBody', () => {
  const tpl = { id: 'base', name: 'Base', description: '', cpu: 1, memory_mb: 512, visibility: 'public' } as unknown as Template
  const base = { tpl, agent: null, accessMode: 'terminal', apiKey: '', baseUrl: '', model: '', mcpJson: '', task: '', testCmd: '', serveReport: false, start: '', port: '', envList: [] }

  for (const kind of ['shell', 'web', 'test'] as const) {
    it(`starts nothing on the preview port for a ${kind} sandbox with no start command`, () => {
      const body = composeCreateBody({ ...base, kind } as unknown as Parameters<typeof composeCreateBody>[0])
      expect(body.start).toBeUndefined()
      expect(JSON.stringify(body.files ?? [])).not.toContain('ttyd')
      expect(body.port).toBe(8000)
    })
  }
})

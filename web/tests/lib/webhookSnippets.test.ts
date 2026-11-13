import { describe, expect, it } from 'vitest'
import { webhookSnippets } from '../../src/lib/webhookSnippets'

const base = { origin: 'https://pilldrops.test', url: 'https://hooks.example.com/in?a=1', events: ['sandbox.ended'] }

describe('webhookSnippets', () => {
  it('builds a curl that adds, tests and lists the org webhooks', () => {
    const { curl, request } = webhookSnippets({ ...base, hookID: 'wh_9' })
    expect(curl).toContain('curl -X POST https://pilldrops.test/v1/webhooks \\')
    expect(curl).toContain(`-d '{"url":"https://hooks.example.com/in?a=1","events":["sandbox.ended"]}'`)
    expect(curl).toContain('https://pilldrops.test/v1/webhooks/wh_9/test')
    expect(curl).toContain('Authorization: Bearer $PILLDROPS_API_KEY')
    expect(request).toContain('"type": "sandbox.ended"')
  })

  it('uses a quoted placeholder until a webhook exists', () => {
    expect(webhookSnippets(base).curl).toContain(`/v1/webhooks/<webhook id>/test'`)
  })
})

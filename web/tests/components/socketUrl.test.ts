import { describe, it, expect } from 'vitest'
import { socketUrl } from '../../src/components/SandboxTerminal'

describe('socketUrl', () => {
  it('serves a relative path from the dashboard origin', () => {
    expect(socketUrl('/preview/sb1/7681/ws', { protocol: 'https:', host: 'pd.example' }))
      .toBe('wss://pd.example/preview/sb1/7681/ws')
    expect(socketUrl('/preview/sb1/7681/ws', { protocol: 'http:', host: 'localhost:8080' }))
      .toBe('ws://localhost:8080/preview/sb1/7681/ws')
  })

  it('keeps the host of a preview on its own sandbox domain', () => {
    expect(socketUrl('https://7681-sb1.localtest.me/ws?token=t', { protocol: 'http:', host: 'localhost:19090' }))
      .toBe('wss://7681-sb1.localtest.me/ws?token=t')
    expect(socketUrl('http://7681-sb1.sb.test/ws', { protocol: 'https:', host: 'pd.example' }))
      .toBe('ws://7681-sb1.sb.test/ws')
  })
})

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, act, waitFor } from '@testing-library/react'
import SandboxTerminal from '../../src/components/SandboxTerminal'

vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80
    rows = 24
    loadAddon() {}
    open() {}
    write() {}
    focus() {}
    dispose() {}
    onData() { return { dispose() {} } }
  },
}))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit() {} } }))

const sockets: FakeSocket[] = []
class FakeSocket {
  static OPEN = 1
  readyState = 0
  binaryType = ''
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onmessage: unknown = null
  onerror: unknown = null
  constructor() { sockets.push(this) }
  send() {}
  close() {}
}

beforeEach(() => {
  sockets.length = 0
  vi.stubGlobal('WebSocket', FakeSocket)
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('SandboxTerminal reconnect', () => {
  it('reconnects a closed session when the browser comes back online', async () => {
    render(<SandboxTerminal wsPath="/term" />)
    act(() => { sockets[0].onclose?.() })
    act(() => { window.dispatchEvent(new Event('online')) })
    await waitFor(() => expect(sockets).toHaveLength(2))
  })

  it('leaves an open session alone when the browser comes back online', () => {
    render(<SandboxTerminal wsPath="/term" />)
    act(() => { window.dispatchEvent(new Event('online')) })
    expect(sockets).toHaveLength(1)
  })
})

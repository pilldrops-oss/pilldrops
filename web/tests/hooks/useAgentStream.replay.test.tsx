import { act, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest'
import { useAgentStream } from '../../src/hooks/useAgentStream'

// A reconnect resumes from a cursor and the server may re-send the frame the cursor points at,
// so an overlap is expected and the client must not double-apply it.

type Handler = (e: MessageEvent) => void

class FakeEventSource {
  static last: FakeEventSource | null = null
  url: string
  onmessage: Handler | null = null
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  listeners: Record<string, Handler[]> = {}
  closed = false

  constructor(url: string) {
    this.url = url
    FakeEventSource.last = this
  }
  addEventListener(type: string, fn: Handler) {
    (this.listeners[type] ??= []).push(fn)
  }
  close() { this.closed = true }

  // Delivers a journal frame the way the server does: a named SSE event.
  emit(type: string, payload: Record<string, unknown>, seq: number) {
    const e = { data: JSON.stringify({ seq, type, payload }) } as MessageEvent
    for (const fn of this.listeners[type] ?? []) fn(e)
  }
}

function Probe({ startSeq }: { startSeq: number }) {
  const s = useAgentStream('sb_1', true, startSeq)
  return (
    <div>
      <span data-testid="text">{s.text}</span>
      <span data-testid="cursor">{s.cursor}</span>
      <span data-testid="count">{s.events.length}</span>
      <span data-testid="truncated">{String(s.truncated)}</span>
    </div>
  )
}

beforeEach(() => {
  FakeEventSource.last = null
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource)
})
afterEach(() => vi.unstubAllGlobals())

describe('useAgentStream replay handling', () => {
  it('does not apply a frame twice when a reconnect replays it', async () => {
    render(<Probe startSeq={0} />)
    const src = FakeEventSource.last!

    act(() => {
      src.emit('turn.output', { text: 'all ' }, 1)
      src.emit('turn.output', { text: 'green' }, 2)
    })
    await waitFor(() => expect(screen.getByTestId('text').textContent).toBe('all green'))

    // The reconnect resends seq 2, the frame the cursor points at.
    act(() => { src.emit('turn.output', { text: 'green' }, 2) })

    expect(screen.getByTestId('text').textContent).toBe('all green')
    expect(screen.getByTestId('count').textContent).toBe('2')
  })

  it('opens at the cursor so a reconnect asks for what it is missing', async () => {
    render(<Probe startSeq={41} />)
    expect(FakeEventSource.last!.url).toContain('since_seq=41')
  })

  it('ignores a frame older than the cursor entirely', async () => {
    render(<Probe startSeq={10} />)
    const src = FakeEventSource.last!

    act(() => { src.emit('turn.output', { text: 'stale' }, 3) })
    expect(screen.getByTestId('text').textContent).toBe('')
    expect(screen.getByTestId('cursor').textContent).toBe('10')
  })

  it('advances the cursor so the next reconnect does not refetch what it has', async () => {
    render(<Probe startSeq={0} />)
    const src = FakeEventSource.last!
    act(() => { src.emit('turn.output', { text: 'x' }, 7) })
    await waitFor(() => expect(screen.getByTestId('cursor').textContent).toBe('7'))
  })

  it('surfaces truncation rather than yielding a clean-looking short stream', async () => {
    render(<Probe startSeq={0} />)
    const src = FakeEventSource.last!
    act(() => {
      src.emit('turn.output', { text: 'partial' }, 1)
      src.emit('turn.output_truncated', { dropped_bytes: 90210 }, 2)
    })
    await waitFor(() => expect(screen.getByTestId('truncated').textContent).toBe('true'))
  })
})

describe('useAgentStream usage', () => {
  function UsageProbe() {
    const s = useAgentStream('sb_1', true, 0)
    return <span data-testid="usage">{JSON.stringify(s.usage ?? null)}</span>
  }

  it('keeps only the fields the agent actually reported', async () => {
    render(<UsageProbe />)
    const src = FakeEventSource.last!
    act(() => {
      src.emit('turn.finished', { input_tokens: 1200, model_cost_usd: 0.031 }, 1)
    })
    await waitFor(() => {
      const u = JSON.parse(screen.getByTestId('usage').textContent!)
      expect(u.inputTokens).toBe(1200)
      expect(u.costUSD).toBe(0.031)
      // Not reported, so absent rather than zero: a 0 would read as "this turn was free".
      expect(u.outputTokens).toBeUndefined()
      expect(u.cacheReadTokens).toBeUndefined()
    })
  })

  it('does not wipe real numbers when the next turn reports nothing', async () => {
    render(<UsageProbe />)
    const src = FakeEventSource.last!
    act(() => { src.emit('turn.finished', { input_tokens: 900 }, 1) })
    await waitFor(() => {
      expect(JSON.parse(screen.getByTestId('usage').textContent!).inputTokens).toBe(900)
    })

    act(() => { src.emit('turn.finished', {}, 2) })
    expect(JSON.parse(screen.getByTestId('usage').textContent!).inputTokens).toBe(900)
  })

  it('reports no usage at all until a turn finishes', async () => {
    render(<UsageProbe />)
    const src = FakeEventSource.last!
    act(() => { src.emit('turn.output', { text: 'working' }, 1) })
    expect(screen.getByTestId('usage').textContent).toBe('null')
  })
})

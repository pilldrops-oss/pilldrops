import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import type { AgentStatus, AgentTurn } from '../../../src/api'

// Every case here is a capability that may legitimately be missing; the UI must say why.

const status = (over: Partial<AgentStatus['session']> = {}): AgentStatus => ({
  sandbox_id: 'sb_1',
  agent: { id: 'claude', name: 'Claude Code' },
  session: {
    state: 'idle', mode: 'rich', queue_depth: 0, last_seq: 10, restarts: 0, ...over,
  },
})

const turn = (over: Partial<AgentTurn> = {}): AgentTurn => ({
  id: 'turn_1', request: 'add tests', state: 'done',
  queued_at: new Date().toISOString(), ...over,
})

// jsdom has no EventSource.
class StubEventSource {
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((e: MessageEvent) => void) | null = null
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

let api: typeof import('../../../src/api')

beforeEach(() => {
  vi.stubGlobal('EventSource', StubEventSource as unknown as typeof EventSource)
})
afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

async function mountWith(
  s: AgentStatus,
  turns: AgentTurn[] = [],
): Promise<typeof import('../../../src/components/agent/AgentPanel')> {
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return {
      ...actual,
      getAgentStatus: vi.fn().mockResolvedValue(s),
      listAgentTurns: vi.fn().mockResolvedValue({ turns }),
      submitAgentTurn: vi.fn().mockResolvedValue({}),
      interruptAgentTurn: vi.fn().mockResolvedValue({}),
      dequeueAgentTurn: vi.fn().mockResolvedValue({}),
      agentEventsURL: actual.agentEventsURL,
    }
  })
  api = await import('../../../src/api')
  return import('../../../src/components/agent/AgentPanel')
}

describe('AgentPanel', () => {
  it('explains that a basic-mode agent has no step detail, rather than showing nothing', async () => {
    const { AgentPanel } = await mountWith(status({ mode: 'basic' }), [turn()])
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByText(/plain output only/i)).toBeInTheDocument()
    })
    expect(screen.getByText(/replies and exit status are complete/i)).toBeInTheDocument()
  })

  it('does not show the basic-mode note for a rich agent', async () => {
    const { AgentPanel } = await mountWith(status({ mode: 'rich' }), [turn()])
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByText('add tests')).toBeInTheDocument())
    expect(screen.queryByText(/plain output only/i)).not.toBeInTheDocument()
  })

  it('always shows the reason a session is degraded', async () => {
    const reason = 'the agent process stopped unexpectedly and was restarted (1 so far): exit 139'
    const { AgentPanel } = await mountWith(status({ state: 'degraded', degraded_reason: reason }))
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => expect(screen.getByText(reason)).toBeInTheDocument())
  })

  it('reports spend as PARTIAL when turns reported no cost', async () => {
    const { AgentPanel } = await mountWith(status({
      spend: {
        cap_usd: 5, spent_usd: 4.1, remaining_usd: 0.9, exceeded: false,
        turns_unreported: 12, coverage: 'Counts model spend the agent reports.',
      },
    }))
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => expect(screen.getByText(/\$4\.10 of \$5\.00/)).toBeInTheDocument())
    expect(screen.getByText(/12 turns reported/i)).toBeInTheDocument()
    expect(screen.getByText(/partial figure/i)).toBeInTheDocument()
  })

  it('omits the spend line entirely when no cap is set', async () => {
    const { AgentPanel } = await mountWith(status())
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByLabelText(/task for the agent/i)).toBeInTheDocument())
    expect(screen.queryByText(/reported model spend/i)).not.toBeInTheDocument()
  })

  it('says the spend cap has frozen the queue, and how to fix it', async () => {
    const { AgentPanel } = await mountWith(status({
      spend: {
        cap_usd: 1, spent_usd: 1, remaining_usd: 0, exceeded: true,
        turns_unreported: 0, coverage: '',
      },
    }))
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByText(/spend cap has been reached/i)).toBeInTheDocument())
    expect(screen.getByText(/raise or clear the cap/i)).toBeInTheDocument()
  })

  it('offers Interrupt only while a turn is running', async () => {
    const { AgentPanel } = await mountWith(status())
    const { unmount } = render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByLabelText(/task for the agent/i)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /interrupt/i })).not.toBeInTheDocument()
    unmount()

    vi.resetModules()
    const running = turn({ id: 'turn_2', request: 'long job', state: 'running', seq_start: 4 })
    const mod = await mountWith(status({ state: 'busy', current_turn: running }), [running])
    render(<mod.AgentPanel sandboxID="sb_1" />)
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /interrupt/i })).toBeInTheDocument())
  })

  it('shows a queue with its one-at-a-time rule stated', async () => {
    const { AgentPanel } = await mountWith(status({ queue_depth: 2 }), [
      turn({ id: 'a', request: 'first', state: 'queued' }),
      turn({ id: 'b', request: 'second', state: 'queued' }),
    ])
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => expect(screen.getByText(/one task runs at a time/i)).toBeInTheDocument())
    expect(screen.getByText(/Queued \(2\)/)).toBeInTheDocument()
  })

  it('tells the user tasks survive closing the tab', async () => {
    const { AgentPanel } = await mountWith(status())
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() =>
      expect(screen.getByText(/close this tab, the task keeps running/i)).toBeInTheDocument())
  })

  it('explains an unreachable supervisor instead of showing a bare error', async () => {
    vi.doMock('../../../src/api', async () => {
      const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
      return {
        ...actual,
        getAgentStatus: vi.fn().mockRejectedValue(new Error('agent_unreachable')),
        listAgentTurns: vi.fn().mockRejectedValue(new Error('agent_unreachable')),
      }
    })
    const { AgentPanel } = await import('../../../src/components/agent/AgentPanel')
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() =>
      expect(screen.getByText(/supervisor is not answering/i)).toBeInTheDocument())
    expect(screen.getByText(/sending a task will wake it/i)).toBeInTheDocument()
  })

  it('renders a failed turn with its exit code and error', async () => {
    const { AgentPanel } = await mountWith(status(), [
      turn({ state: 'failed', exit_code: 1, error: 'credit balance too low' }),
    ])
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByText(/exit 1/)).toBeInTheDocument())
    expect(screen.getByText(/credit balance too low/)).toBeInTheDocument()
  })

  it('states when an agent cannot remember earlier messages', async () => {
    const s = status()
    s.agent = {
      id: 'grok', name: 'Grok', remembers: false,
      memory_note: "This agent's CLI has no resume command, so each message starts a new conversation.",
    }
    const { AgentPanel } = await mountWith(s)
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() =>
      expect(screen.getByText(/no resume command/i)).toBeInTheDocument())
  })

  it('says nothing about memory for an agent that has it', async () => {
    const s = status()
    s.agent = { id: 'claude', name: 'Claude Code', remembers: true }
    const { AgentPanel } = await mountWith(s)
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => expect(screen.getByLabelText(/task for the agent/i)).toBeInTheDocument())
    expect(screen.queryByText(/no resume command/i)).not.toBeInTheDocument()
  })

  it('keeps the API surface it depends on', async () => {
    await mountWith(status())
    for (const fn of ['getAgentStatus', 'listAgentTurns', 'submitAgentTurn',
      'interruptAgentTurn', 'dequeueAgentTurn', 'agentEventsURL'] as const) {
      expect(typeof api[fn]).toBe('function')
    }
  })
})

describe('AgentPanel mode ladder', () => {
  it('offers Guided only when the server says the gate works', async () => {
    const { AgentPanel } = await mountWith(status({
      autonomy: { available: ['supervised', 'guided', 'autonomous'], default: 'autonomous' },
    }))
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => screen.getByLabelText(/how much should it do on its own/i))
    const opts = Array.from(screen.getByRole('combobox').querySelectorAll('option'))
      .map(o => (o as HTMLOptionElement).value)
    expect(opts).toContain('guided')
    // Narrowest first.
    expect(opts.indexOf('supervised')).toBeLessThan(opts.indexOf('autonomous'))
  })

  it('says why Guided is missing rather than hiding it silently', async () => {
    const { AgentPanel } = await mountWith(status({
      autonomy: {
        available: ['supervised', 'autonomous'],
        default: 'autonomous',
        guided_unavailable_reason: 'this agent has no verified approval gate',
      },
    }))
    render(<AgentPanel sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByText(/Guided mode is not available here/i)).toBeTruthy()
      expect(screen.getByText(/no verified approval gate/i)).toBeTruthy()
    })
    const opts = Array.from(screen.getByRole('combobox').querySelectorAll('option'))
      .map(o => (o as HTMLOptionElement).value)
    expect(opts).not.toContain('guided')
  })

  it('sends no mode until one is chosen, so the server default applies', async () => {
    const { AgentPanel } = await mountWith(status({
      autonomy: { available: ['supervised', 'guided', 'autonomous'], default: 'autonomous' },
    }))
    render(<AgentPanel sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByLabelText(/task for the agent/i))
    await user.type(screen.getByLabelText(/task for the agent/i), 'do it')
    await user.click(screen.getByRole('button', { name: /^Send$/i }))

    await waitFor(() => expect(api.submitAgentTurn).toHaveBeenCalledWith('sb_1', 'do it', {}))
  })

  it('sends the chosen mode', async () => {
    const { AgentPanel } = await mountWith(status({
      autonomy: { available: ['supervised', 'guided', 'autonomous'], default: 'autonomous' },
    }))
    render(<AgentPanel sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByRole('combobox'))
    await user.selectOptions(screen.getByRole('combobox'), 'guided')
    await user.type(screen.getByLabelText(/task for the agent/i), 'review this')
    await user.click(screen.getByRole('button', { name: /^Send$/i }))

    await waitFor(() =>
      expect(api.submitAgentTurn).toHaveBeenCalledWith('sb_1', 'review this', { mode: 'guided' }))
  })

  it('renders no picker at all for an agentd too old to report a ladder', async () => {
    // agentd upgrades on its own schedule; a missing ladder means offer no choice, not all of them.
    const { AgentPanel } = await mountWith(status())
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => screen.getByLabelText(/task for the agent/i))
    expect(screen.queryByRole('combobox')).toBeNull()
  })
})

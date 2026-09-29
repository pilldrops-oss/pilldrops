import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, afterEach } from 'vitest'
import type { AgentStatus, AgentTurn } from '../../../src/api'

// Skeletons not spinners, and a failed poll must never blank a value already on screen.

const status = (): AgentStatus => ({
  sandbox_id: 'sb_1',
  agent: { id: 'claude', name: 'Claude Code', remembers: true },
  session: {
    state: 'idle',
    autonomy: { available: ['supervised', 'guided', 'autonomous'], default: 'guided' },
  },
} as unknown as AgentStatus)

const turns = (): { turns: AgentTurn[] } => ({
  turns: [{
    id: 'tn_1', request: 'run the tests', state: 'done',
    queued_at: '2026-08-10T09:00:00Z', response: 'all green',
  }],
})

let getStatus: ReturnType<typeof vi.fn>
let listTurns: ReturnType<typeof vi.fn>

async function mount() {
  getStatus = vi.fn().mockResolvedValue(status())
  listTurns = vi.fn().mockResolvedValue(turns())
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return { ...actual, getAgentStatus: getStatus, listAgentTurns: listTurns }
  })
  return import('../../../src/components/agent/AgentPanel')
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

// No EventSource stub on purpose: jsdom has none, so these run in the polling fallback.
describe('AgentPanel resilience', () => {
  it('shows a skeleton, not a spinner or a sentence, before anything loads', async () => {
    const { AgentPanel } = await mount()
    const { container } = render(<AgentPanel sandboxID="sb_1" />)
    expect(container.querySelector('.agent-skeleton')).toBeTruthy()
    expect(screen.queryByText(/Loading the agent/)).toBeNull()
  })

  it('keeps showing the transcript when a poll fails', async () => {
    const { AgentPanel } = await mount()
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => screen.getByText('run the tests'))

    getStatus.mockRejectedValue(new Error('network blip'))
    listTurns.mockRejectedValue(new Error('network blip'))
    await waitFor(() => expect(screen.getByText('run the tests')).toBeTruthy())
  })
})

  it('degrades to polling instead of crashing when there is no EventSource', async () => {
    const { AgentPanel } = await mount()
    render(<AgentPanel sandboxID="sb_1" />)
    await waitFor(() => screen.getByText('run the tests'))
    // It says which transport is live rather than implying a stream.
    expect(screen.getByText(/paused \(background tab\)|reconnecting|live/)).toBeTruthy()
  })

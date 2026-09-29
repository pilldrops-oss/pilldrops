import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, afterEach } from 'vitest'
import type { IsolationFact } from '../../../src/api'

// Every test asks whether the card can show a reassuring answer the server did not give.

async function mount(facts: IsolationFact[] | Error) {
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return {
      ...actual,
      getAgentIsolation: facts instanceof Error
        ? vi.fn().mockRejectedValue(facts)
        : vi.fn().mockResolvedValue({ sandbox_id: 'sb_1', facts }),
    }
  })
  return import('../../../src/components/agent/IsolationCard')
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('IsolationCard', () => {
  it('shows a ✗ when the node is not a microVM, instead of the marketing claim', async () => {
    const { IsolationCard } = await mount([{
      label: 'Hardware-virtualised microVM',
      state: 'no',
      detail: 'This node runs the container runtime: isolation is kernel namespaces and cgroups.',
    }])
    render(<IsolationCard sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('Hardware-virtualised microVM'))
    // The reason is inline, not a tooltip; touch screens have no hover.
    expect(screen.getByText('✗')).toBeTruthy()
    expect(screen.getByText(/kernel namespaces/)).toBeTruthy()
    expect(screen.queryByText('✓')).toBeNull()
  })

  it('renders "unknown" as its own state rather than as a failure', async () => {
    const { IsolationCard } = await mount([{
      label: 'Encrypted in transit',
      state: 'unknown',
      detail: 'This request arrived without TLS and without a proxy protocol header.',
    }])
    render(<IsolationCard sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('Encrypted in transit'))
    expect(screen.getByText('?')).toBeTruthy()
    // In words too, for screen readers.
    expect(screen.getByText('could not be verified')).toBeTruthy()
    expect(screen.queryByText('✗')).toBeNull()
  })

  it('says it could not check, instead of rendering an empty card that reads as "all clear"', async () => {
    const { IsolationCard } = await mount(new Error('sandbox is paused'))
    render(<IsolationCard sandboxID="sb_1" />)

    await waitFor(() => screen.getByText(/Could not check/))
    expect(screen.getByText(/sandbox is paused/)).toBeTruthy()
    expect(screen.queryByText('✓')).toBeNull()
  })

  it('shows no checkmark before anything has been measured', async () => {
    const { IsolationCard } = await mount([])
    render(<IsolationCard sandboxID="sb_1" />)
    expect(screen.queryByText('✓')).toBeNull()
    expect(screen.queryByText('✗')).toBeNull()
    expect(screen.queryByText('?')).toBeNull()
  })

  it('renders every fact the server sent, including the unflattering ones', async () => {
    const { IsolationCard } = await mount([
      { label: 'Hardware-virtualised microVM', state: 'yes', detail: 'A Firecracker VM.' },
      { label: 'Provider key not stored', state: 'no', detail: 'This workstation has a stored credential (ANTHROPIC_API_KEY).' },
      { label: 'Autonomy: autonomous', state: 'no', detail: 'The agent acts without asking.' },
    ])
    render(<IsolationCard sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('Hardware-virtualised microVM'))
    expect(screen.getByText('Provider key not stored')).toBeTruthy()
    expect(screen.getByText('Autonomy: autonomous')).toBeTruthy()
    expect(screen.getAllByText('✗')).toHaveLength(2)
  })
})

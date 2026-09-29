import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, afterEach } from 'vitest'
import type { AgentTurn } from '../../../src/api'

// Three silent failures: an unknown cost shown as $0.00, mirrored partial history
// shown as the whole truth, and a failed poll blanking the list.

const turn = (over: Partial<AgentTurn> = {}): AgentTurn => ({
  id: 'tn_1',
  request: 'run the tests',
  state: 'done',
  queued_at: '2026-08-10T09:00:00Z',
  started_at: '2026-08-10T09:00:01Z',
  ended_at: '2026-08-10T09:00:31Z',
  response: 'all green',
  ...over,
})

let list: ReturnType<typeof vi.fn>

async function mount(
  res: { turns: AgentTurn[]; served_from?: 'node' | 'mirror'; note?: string } | Error,
) {
  list = res instanceof Error
    ? vi.fn().mockRejectedValue(res)
    : vi.fn().mockResolvedValue(res)
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return { ...actual, listAgentTurns: list }
  })
  return import('../../../src/components/agent/TimelineTab')
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('TimelineTab', () => {
  it('says a cost is not reported rather than showing $0.00', async () => {
    const { TimelineTab } = await mount({ turns: [turn()], served_from: 'node' })
    render(<TimelineTab sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('run the tests'))
    expect(screen.getByText('cost not reported')).toBeTruthy()
    expect(screen.queryByText('$0.0000')).toBeNull()
  })

  it('shows a real cost when the agent reported one', async () => {
    const { TimelineTab } = await mount({ turns: [turn({ cost_usd: 0.0231 })] })
    render(<TimelineTab sandboxID="sb_1" />)
    await waitFor(() => screen.getByText('$0.0231'))
  })

  it('labels history that came from the mirror, in the server’s own words', async () => {
    const { TimelineTab } = await mount({
      turns: [turn()],
      served_from: 'mirror',
      note: 'this workstation is not running; these turns come from the control plane’s copy',
    })
    render(<TimelineTab sandboxID="sb_1" />)

    await waitFor(() => screen.getByText(/control plane/))
  })

  it('does not label live history as mirrored', async () => {
    const { TimelineTab } = await mount({ turns: [turn()], served_from: 'node' })
    render(<TimelineTab sandboxID="sb_1" />)
    await waitFor(() => screen.getByText('run the tests'))
    expect(screen.queryByText(/control plane/)).toBeNull()
  })

  it('names who started the turn', async () => {
    const { TimelineTab } = await mount({
      turns: [turn({ source: 'cron' }), turn({ id: 'tn_2', source: 'token:sfa_x' })],
    })
    render(<TimelineTab sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('schedule'))
    // A token id is a credential identifier; keep it off screen.
    expect(screen.getByText('API token')).toBeTruthy()
    expect(screen.queryByText(/sfa_x/)).toBeNull()
  })

  it('explains an empty response instead of rendering a blank box', async () => {
    const { TimelineTab } = await mount({
      turns: [turn({ state: 'awaiting_approval', response: undefined, ended_at: undefined })],
    })
    render(<TimelineTab sandboxID="sb_1" />)

    await waitFor(() => screen.getByText('run the tests'))
    expect(screen.getByText('waiting for you')).toBeTruthy()
    await userEvent.click(screen.getByText('run the tests'))
    expect(screen.getByText(/needs your approval/)).toBeTruthy()
  })

  it('says nothing has run yet, without implying the feature is broken', async () => {
    const { TimelineTab } = await mount({ turns: [] })
    render(<TimelineTab sandboxID="sb_1" />)
    await waitFor(() => screen.getByText(/Nothing yet/))
  })

  it('keeps showing the last good list when a poll fails', async () => {
    const { TimelineTab } = await mount({ turns: [turn()], served_from: 'node' })
    render(<TimelineTab sandboxID="sb_1" />)
    await waitFor(() => screen.getByText('run the tests'))

    list.mockRejectedValueOnce(new Error('network down'))
    await waitFor(() => expect(screen.getByText('run the tests')).toBeTruthy())
  })
})

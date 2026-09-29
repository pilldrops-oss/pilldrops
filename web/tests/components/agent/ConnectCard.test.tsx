import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, afterEach } from 'vitest'
import type { AgentConnectBundle } from '../../../src/api'

// Fetching mints a token, so it must not happen on render; and every channel is
// either usable or says why not.

const bundle = (over: Partial<AgentConnectBundle> = {}): AgentConnectBundle => ({
  sandbox_id: 'sb_1',
  agent: { id: 'claude', name: 'Claude Code' },
  token: {
    id: 'sfa_abc',
    scopes: ['read', 'write'],
    value: 'sfa_abc_secretvaluehere0123456789',
    shown_once: true,
    expires_at: new Date(Date.now() + 86400_000).toISOString(),
    note: 'Shown once.',
  },
  endpoints: { status: 'https://x/status' },
  snippets: { curl_submit: 'curl -X POST https://x/turns' },
  mcp: { config: '{"mcpServers":{}}', transport: 'http' },
  origin: { url: 'https://agent-sb1.example.com', note: 'A separate origin per sandbox.' },
  ...over,
})

let fetchBundle: ReturnType<typeof vi.fn>

async function mount(b: AgentConnectBundle | Error = bundle()) {
  fetchBundle = b instanceof Error
    ? vi.fn().mockRejectedValue(b)
    : vi.fn().mockResolvedValue(b)
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return { ...actual, getAgentConnect: fetchBundle }
  })
  return import('../../../src/components/agent/ConnectCard')
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('ConnectCard', () => {
  it('does not mint a token until asked', async () => {
    const { ConnectCard } = await mount()
    render(<ConnectCard sandboxID="sb_1" />)
    expect(fetchBundle).not.toHaveBeenCalled()
    expect(screen.getByText(/Get connection details/)).toBeTruthy()
  })

  it('warns that the token is shown once BEFORE showing it', async () => {
    const { ConnectCard } = await mount()
    render(<ConnectCard sandboxID="sb_1" />)
    await userEvent.click(screen.getByText(/Get connection details/))

    const warning = await waitFor(() => screen.getByText(/shown once/i))
    const value = screen.getByText(/•/)
    // compareDocumentPosition's FOLLOWING bit is 4.
    expect(warning.compareDocumentPosition(value) & 4).toBeTruthy()
  })

  it('masks the token on screen while copying it in full', async () => {
    const { ConnectCard } = await mount()
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(<ConnectCard sandboxID="sb_1" />)
    await userEvent.click(screen.getByText(/Get connection details/))
    await waitFor(() => screen.getByText(/shown once/i))

    expect(screen.queryByText('sfa_abc_secretvaluehere0123456789')).toBeNull()

    const copyButtons = screen.getAllByLabelText('Copy')
    await userEvent.click(copyButtons[0])
    expect(writeText).toHaveBeenCalledWith('sfa_abc_secretvaluehere0123456789')
  })

  it('lists an unavailable channel with its reason rather than hiding it', async () => {
    const { ConnectCard } = await mount(bundle({
      channels: [
        { id: 'telegram', name: 'Telegram', status: 'ready' },
        { id: 'slack', name: 'Slack', status: 'unavailable', reason: 'no bot token configured' },
      ],
    }))
    render(<ConnectCard sandboxID="sb_1" />)
    await userEvent.click(screen.getByText(/Get connection details/))

    await waitFor(() => screen.getByText('Telegram'))
    expect(screen.getByText('Slack')).toBeTruthy()
    expect(screen.getByText(/no bot token configured/)).toBeTruthy()
  })

  it('tells the user to select manually when the clipboard is unavailable', async () => {
    const { ConnectCard } = await mount()
    // navigator.clipboard is absent over plain HTTP and can be denied by permissions policy.
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockRejectedValue(new Error('denied')) } })

    render(<ConnectCard sandboxID="sb_1" />)
    await userEvent.click(screen.getByText(/Get connection details/))
    await waitFor(() => screen.getByText(/shown once/i))
    await userEvent.click(screen.getAllByLabelText('Copy')[0])

    await waitFor(() => screen.getByText(/Select it manually/))
  })

  it('reports a failure to mint instead of showing an empty card', async () => {
    const { ConnectCard } = await mount(new Error('the workstation is paused'))
    render(<ConnectCard sandboxID="sb_1" />)
    await userEvent.click(screen.getByText(/Get connection details/))
    await waitFor(() => screen.getByText(/the workstation is paused/))
  })
})

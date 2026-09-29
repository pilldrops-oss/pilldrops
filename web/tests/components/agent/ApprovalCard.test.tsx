import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import type { AgentApproval, AgentApprovalsResponse } from '../../../src/api'

// The gate denies on timeout, so every test asks whether a reviewer can be
// misled into an unintended refusal or a wider allow than they read.

const approval = (over: Partial<AgentApproval> = {}): AgentApproval => ({
  id: 'apr_1',
  turn_id: 'turn_1',
  tool: 'Bash',
  tool_use_id: 'toolu_1',
  signature: 'Bash: npm install *',
  args_digest: 'a'.repeat(64),
  args_preview: '{"command":"npm install left-pad"}',
  requested_at: new Date().toISOString(),
  ...over,
})

const response = (over: Partial<AgentApprovalsResponse> = {}): AgentApprovalsResponse => ({
  approvals: [approval()],
  remember_scopes: ['once', 'session', 'sandbox'],
  timeout_seconds: 900,
  on_timeout: 'deny',
  can_decide: true,
  ...over,
})

let decide: ReturnType<typeof vi.fn>

async function mount(res: AgentApprovalsResponse | Error) {
  decide = vi.fn().mockResolvedValue({ decided: 'apr_1', decision: 'allow' })
  vi.doMock('../../../src/api', async () => {
    const actual = await vi.importActual<typeof import('../../../src/api')>('../../../src/api')
    return {
      ...actual,
      listAgentApprovals: res instanceof Error
        ? vi.fn().mockRejectedValue(res)
        : vi.fn().mockResolvedValue(res),
      decideAgentApproval: decide,
    }
  })
  return import('../../../src/components/agent/ApprovalCard')
}

beforeEach(() => vi.useRealTimers())
afterEach(() => {
  vi.restoreAllMocks()
  vi.resetModules()
})

describe('ApprovalQueue', () => {
  it('says out loud that not answering is a refusal', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByText(/not answered in time is refused, not allowed/i)).toBeTruthy()
    })
  })

  it('shows a countdown so the deadline is not a surprise', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByRole('timer').textContent).toMatch(/Refused in .* if nobody answers/)
    })
  })

  it('leads with the command being run, not with the JSON payload', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByText('npm install left-pad')).toBeTruthy()
    })
    // The payload starts collapsed.
    expect(screen.queryByText(/"command"/)).toBeNull()
  })

  it('names the category a remembered allow would cover', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => {
      const sigs = screen.getAllByText('Bash: npm install *')
      expect(sigs.length).toBeGreaterThanOrEqual(2) // session + sandbox options
    })
  })

  it('defaults to the narrowest scope', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => screen.getByText(/just this request/i))
    const once = screen.getByRole('radio', { name: /just this request/i }) as HTMLInputElement
    expect(once.checked).toBe(true)
    for (const name of [/until the agent restarts/i, /for this workstation/i]) {
      expect((screen.getByRole('radio', { name }) as HTMLInputElement).checked).toBe(false)
    }
  })

  it('sends the chosen scope with an allow, and never with a refusal', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByText(/until the agent restarts/i))
    await user.click(screen.getByRole('radio', { name: /until the agent restarts/i }))
    await user.click(screen.getByRole('button', { name: /^Allow$/i }))

    await waitFor(() => expect(decide).toHaveBeenCalledWith('sb_1', 'apr_1', 'allow', 'session'))
  })

  it('refuses only the one request, whatever scope is selected', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByText(/for this workstation/i))
    await user.click(screen.getByRole('radio', { name: /for this workstation/i }))
    await user.click(screen.getByRole('button', { name: /refuse/i }))

    // A remembered denial has no prompt and no obvious undo, so a refusal is always `once`.
    await waitFor(() => expect(decide).toHaveBeenCalledWith('sb_1', 'apr_1', 'deny', 'once'))
  })

  it('discloses that the shown payload is redacted and may be cut short', async () => {
    const { ApprovalQueue } = await mount(response())
    render(<ApprovalQueue sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByRole('button', { name: /show what it sent/i }))
    await user.click(screen.getByRole('button', { name: /show what it sent/i }))

    expect(screen.getByText(/Secrets are removed and long payloads are shortened/i)).toBeTruthy()
  })

  it('tells a read-only session it cannot answer, instead of showing buttons that 403', async () => {
    const { ApprovalQueue } = await mount(response({
      can_decide: false,
      cannot_decide_reason: 'this token is marked as automation, and automation cannot approve',
    }))
    render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => {
      expect(screen.getByText(/automation cannot approve/i)).toBeTruthy()
    })
    expect(screen.queryByRole('button', { name: /refuse/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /^Allow/i })).toBeNull()
  })

  it('renders nothing at all when the agent has no gate', async () => {
    // A 501, not an empty queue; "nothing to review" would be false for an ungated agent.
    const { ApprovalQueue } = await mount(new Error('this agent has no approval gate'))
    const { container } = render(<ApprovalQueue sandboxID="sb_1" />)

    await waitFor(() => expect(container.textContent).toBe(''))
  })

  it('renders nothing when there is nothing waiting', async () => {
    const { ApprovalQueue } = await mount(response({ approvals: [] }))
    const { container } = render(<ApprovalQueue sandboxID="sb_1" />)
    await waitFor(() => expect(container.textContent).toBe(''))
  })

  it('says the answer did not count when it raced the timeout', async () => {
    const { ApprovalQueue } = await mount(response())
    decide.mockRejectedValueOnce(new Error('this approval is no longer pending'))
    render(<ApprovalQueue sandboxID="sb_1" />)
    const user = userEvent.setup()

    await waitFor(() => screen.getByRole('button', { name: /refuse/i }))
    await user.click(screen.getByRole('button', { name: /refuse/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert').textContent)
        .toMatch(/timed out before your answer arrived/i)
    })
  })

  it('pluralises the header so a queue of several is not read as one', async () => {
    const { ApprovalQueue } = await mount(response({
      approvals: [approval(), approval({ id: 'apr_2', tool: 'Write' })],
    }))
    render(<ApprovalQueue sandboxID="sb_1" />)
    await waitFor(() => {
      expect(screen.getByText(/2 actions are waiting/i)).toBeTruthy()
    })
  })
})

describe('targetOf', () => {
  it('picks the field a reviewer actually judges', async () => {
    const { targetOf } = await mount(response())
    expect(targetOf(approval({ args_preview: '{"command":"rm -rf /work"}' }))).toBe('rm -rf /work')
    expect(targetOf(approval({ args_preview: '{"file_path":"/work/x.go","content":"…"}' })))
      .toBe('/work/x.go')
    expect(targetOf(approval({ args_preview: '{"url":"https://evil.example.com"}' })))
      .toBe('https://evil.example.com')
  })

  it('falls back to the signature when the preview was truncated', async () => {
    const { targetOf } = await mount(response())
    // Previews are cut to a budget, so a truncated one is not valid JSON.
    expect(targetOf(approval({ args_preview: '{"content":"aaaa… (900 bytes trunc' })))
      .toBe('Bash: npm install *')
  })
})

describe('formatLeft', () => {
  it('reads as time a person can act on', async () => {
    const { formatLeft } = await mount(response())
    expect(formatLeft(900)).toBe('15m')
    expect(formatLeft(95)).toBe('1m 35s')
    expect(formatLeft(42)).toBe('42s')
  })
})

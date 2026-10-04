import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import AddNode from '../../src/pages/AddNode'
import * as api from '../../src/api'

vi.mock('../../src/api', () => ({
  mintJoinToken: vi.fn(),
  listNodes: vi.fn(),
}))

beforeEach(() => {
  vi.mocked(api.listNodes).mockReset().mockResolvedValue({ nodes: [] })
  vi.mocked(api.mintJoinToken).mockReset().mockResolvedValue({
    token: 'tok', expires_at: Math.floor(Date.now() / 1000) + 1800,
    bootstrap_cmd: 'curl -fsSL http://localhost:19090/install.sh -o sf-install.sh && '
      + 'ORCHESTRATOR_URL=http://localhost:19090 sh sf-install.sh',
  })
})

// B26: the command's ORCHESTRATOR_URL is the browser's own address (there is
// no server-provided public URL to use instead), so the field is called out
// and labeled rather than left to blend into the command string.
describe('AddNode join command (B26)', () => {
  it('labels the ORCHESTRATOR_URL value the command carries', async () => {
    const user = userEvent.setup()
    render(<AddNode />)

    await user.click(screen.getByRole('button', { name: /generate join command/i }))

    expect(await screen.findByText(/orchestrator url/i)).toBeInTheDocument()
    expect(screen.getByText('http://localhost:19090')).toBeInTheDocument()
  })
})

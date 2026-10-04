import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import ClusterSandboxes from '../../src/pages/ClusterSandboxes'
import * as api from '../../src/api'
import type { SandboxRow } from '../../src/types'

vi.mock('../../src/api', () => ({
  listSandboxes: vi.fn(),
}))

const row = (over: Partial<SandboxRow> = {}): SandboxRow => ({
  id: 'sb1', template: 'node', status: 'running', boot_type: '', boot_ms: 0,
  created_at: '2026-01-01T00:00:00Z', expires_at: '', ...over,
})

// S4 plan item: the cluster view shows Node only when the list carries it
// (an operator-role caller); a server too old or a caller without the role
// never sends the field, and the column must not render as a bare "—".
describe('ClusterSandboxes node column', () => {
  it('shows Node when the list carries it', async () => {
    vi.mocked(api.listSandboxes).mockResolvedValue({ sandboxes: [row({ node: 'node-4' })] })
    render(<ClusterSandboxes />)
    expect(await screen.findByText('Node')).toBeInTheDocument()
    expect(await screen.findByText('node-4')).toBeInTheDocument()
  })

  it('hides Node when no row carries it', async () => {
    vi.mocked(api.listSandboxes).mockResolvedValue({ sandboxes: [row({})] })
    render(<ClusterSandboxes />)
    await screen.findByText('sb1')
    expect(screen.queryByText('Node')).toBeNull()
  })
})

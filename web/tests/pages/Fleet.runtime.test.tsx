import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import Fleet from '../../src/pages/Fleet'
import { ToastProvider } from '../../src/components/Toast'
import * as api from '../../src/api'
import type { NodeInfo } from '../../src/types'

vi.mock('../../src/api', () => ({
  listNodes: vi.fn(),
  getClusterStats: vi.fn(),
  cordonNode: vi.fn(),
  uncordonNode: vi.fn(),
  drainNode: vi.fn(),
  removeNode: vi.fn(),
}))

const node = (over: Partial<NodeInfo> = {}): NodeInfo => ({
  id: 'node-a', url: 'http://node-a', zone: 'default', healthy: true, status: 'available',
  first_seen: 0, last_seen: 0,
  capacity: {
    cpu_cores: 2, cpu_percent: 10, load1: 0.2, mem_total_mb: 4096, mem_available_mb: 2048,
    mem_committed_mb: 1024, disk_free_gb: 50, sandboxes_active: 1,
  },
  ...over,
})

beforeEach(() => {
  vi.mocked(api.getClusterStats).mockReset().mockResolvedValue(undefined as never)
})

// B10 plan item: Fleet page shows each node's runtime.
describe('Fleet node runtime', () => {
  it('shows the runtime a node reports', async () => {
    vi.mocked(api.listNodes).mockResolvedValue({ nodes: [node({ runtime: 'firecracker' })] })
    render(<ToastProvider><Fleet /></ToastProvider>)
    expect(await screen.findByText(/runtime · firecracker/)).toBeInTheDocument()
  })

  it('shows no runtime chip for a node that does not report one', async () => {
    vi.mocked(api.listNodes).mockResolvedValue({ nodes: [node({ runtime: undefined })] })
    render(<ToastProvider><Fleet /></ToastProvider>)
    await screen.findByText('node-a')
    expect(screen.queryByText(/runtime ·/)).toBeNull()
  })
})

import { describe, it, expect, vi } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { useLaunchRun } from '../../src/hooks/useLaunchRun'
import * as api from '../../src/api'
import type { CreateSandboxReq, Template } from '../../src/types'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  createSandbox: vi.fn(),
  destroySandbox: vi.fn(),
  getPreviewStatus: vi.fn(),
  getSandbox: vi.fn(),
  postLaunchSample: vi.fn(),
}))

const tpl: Template = { id: 'node', name: 'node', description: '', cpu: 1, memory_mb: 512, visibility: 'public' }
const body: CreateSandboxReq = { template: 'node' }

// U5: a create failure (a docker port refusal, an egress allowlist error, …)
// must show the server's own message, not a bare ".message" that can read
// "undefined" when the rejection is not a plain Error.
describe('useLaunchRun create errors', () => {
  it('shows the server message from an ApiError unchanged', async () => {
    vi.mocked(api.createSandbox).mockRejectedValue(
      new api.ApiError({ status: 501, method: 'POST', path: '/v1/sandboxes', message: 'egress_not_enforced: use runtime: firecracker' }),
    )
    const { result } = renderHook(() => useLaunchRun(undefined))
    await act(async () => { await result.current.run(body, tpl) })
    await waitFor(() => expect(result.current.phase).toBe('error'))
    expect(result.current.error).toBe('egress_not_enforced: use runtime: firecracker')
  })

  it('does not render "undefined" for a rejection that is not an Error', async () => {
    vi.mocked(api.createSandbox).mockRejectedValue('boom')
    const { result } = renderHook(() => useLaunchRun(undefined))
    await act(async () => { await result.current.run(body, tpl) })
    await waitFor(() => expect(result.current.phase).toBe('error'))
    expect(result.current.error).toBe('boom')
  })
})

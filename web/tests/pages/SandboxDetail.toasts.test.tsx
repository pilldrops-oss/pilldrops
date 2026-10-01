import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import SandboxDetail from '../../src/pages/SandboxDetail'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'
import { ApiError } from '../../src/api'
import type { SandboxDetail as SB } from '../../src/types'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  getSandbox: vi.fn(),
  listTemplates: vi.fn(),
  getSandboxMetrics: vi.fn(),
  getPreviewToken: vi.fn(),
  pauseSandbox: vi.fn(),
  refreshSandbox: vi.fn(),
  setSandboxSlug: vi.fn(),
}))
vi.mock('../../src/components/SandboxTerminal', () => ({ default: () => <div data-testid="pty" /> }))

const original = window.__SANDBOXD__
const sb: SB = {
  id: 'sb1', template: 'node', name: 'my-app', status: 'running', boot_type: '', boot_ms: 0,
  created_at: '2026-10-01T00:00:00Z', expires_at: '2026-10-02T00:00:00Z',
  storage_mb: 2048, url: '/preview/sb1/3000/', visibility: 'org',
}

beforeEach(() => {
  window.__SANDBOXD__ = { key: '' } as typeof window.__SANDBOXD__
  vi.mocked(api.getSandbox).mockResolvedValue(sb)
  vi.mocked(api.listTemplates).mockResolvedValue([])
  vi.mocked(api.getSandboxMetrics).mockResolvedValue(null as never)
  vi.mocked(api.getPreviewToken).mockResolvedValue('tok')
})
afterEach(() => {
  window.__SANDBOXD__ = original
  vi.restoreAllMocks()
})

function renderPage() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <MemoryRouter initialEntries={['/sandbox/sb1']}>
          <Routes><Route path="/sandbox/:id" element={<SandboxDetail />} /></Routes>
        </MemoryRouter>
      </ConfirmProvider>
    </ToastProvider>,
  )
}

describe('sandbox detail success toasts (B22 toasts item)', () => {
  it('shows a success toast after pausing', async () => {
    vi.mocked(api.pauseSandbox).mockResolvedValue({ status: 'paused' } as never)
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Pause' }))
    expect(await screen.findByText('Paused')).toBeInTheDocument()
  })

  it('shows a success toast after refreshing the TTL', async () => {
    vi.mocked(api.refreshSandbox).mockResolvedValue({ status: 'running' } as never)
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Refresh TTL' }))
    expect(await screen.findByText('TTL refreshed')).toBeInTheDocument()
  })

  it('shows a success toast after saving the slug', async () => {
    vi.mocked(api.setSandboxSlug).mockResolvedValue({ ...sb, slug: 'myapp' })
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Info' }))
    const input = await screen.findByLabelText('Sandbox slug')
    await userEvent.type(input, 'myapp')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('Slug set to myapp')).toBeInTheDocument()
  })
})

describe('sandbox detail polling stops on 404 (B22)', () => {
  it('stops polling once the sandbox is gone', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const notFound = new ApiError({ status: 404, method: 'GET', path: '/v1/sandboxes/sb1', message: 'Not found' })
    vi.mocked(api.getSandbox).mockResolvedValueOnce(sb).mockRejectedValue(notFound)
    renderPage()
    await vi.waitFor(() => expect(api.getSandbox).toHaveBeenCalledTimes(1))

    await vi.advanceTimersByTimeAsync(5000)
    await vi.waitFor(() => expect(api.getSandbox).toHaveBeenCalledTimes(2))

    await vi.advanceTimersByTimeAsync(20000)
    expect(api.getSandbox).toHaveBeenCalledTimes(2)
    vi.useRealTimers()
  })
})

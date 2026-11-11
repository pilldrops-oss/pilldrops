import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
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
  setPreviewAccess: vi.fn(),
}))
vi.mock('../../src/components/SandboxTerminal', () => ({ default: () => <div data-testid="pty" /> }))

const original = window.__SANDBOXD__
const sb: SB = {
  id: 'sb1', template: 'node', name: 'my-app', status: 'running', boot_type: '', boot_ms: 0,
  created_at: '2026-10-01T00:00:00Z', expires_at: '2026-10-02T00:00:00Z',
  storage_mb: 2048, url: '/preview/sb1/3000/', visibility: 'org', public_preview: false,
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

// B24: preview access can be flipped after launch, separately from who may
// use the sandbox's API (visibility).
describe('sandbox detail preview access', () => {
  it('flips the preview to public and re-reads the sandbox', async () => {
    vi.mocked(api.setPreviewAccess).mockResolvedValue({ id: 'sb1', public_preview: true, was: false })
    renderPage()

    const select = await screen.findByRole('combobox', { name: 'Preview' })
    expect(select).toHaveValue('token')
    await userEvent.selectOptions(select, 'public')

    await waitFor(() => expect(api.setPreviewAccess).toHaveBeenCalledWith('sb1', true))
    expect(await screen.findByText('Preview is now public')).toBeInTheDocument()
  })

  it('names the terminal and desktop ports as always needing a token', async () => {
    renderPage()
    const select = await screen.findByRole('combobox', { name: 'Preview' })
    expect(select.closest('label')?.getAttribute('title')).toMatch(/7681.*terminal.*6080.*desktop/)
  })

  it('hides the control once the server says this caller may not change it', async () => {
    vi.mocked(api.setPreviewAccess).mockRejectedValue(
      new ApiError({ status: 404, method: 'POST', path: '/v1/sandboxes/sb1/preview-access', message: 'sandbox not found' }),
    )
    renderPage()

    const select = await screen.findByRole('combobox', { name: 'Preview' })
    await userEvent.selectOptions(select, 'public')

    await waitFor(() => expect(screen.queryByRole('combobox', { name: 'Preview' })).toBeNull())
  })

  it('is absent when the server has no public-preview concept', async () => {
    vi.mocked(api.getSandbox).mockResolvedValue({ ...sb, public_preview: undefined })
    renderPage()
    await screen.findByText('my-app', { selector: '.detail-name' })
    expect(screen.queryByRole('combobox', { name: 'Preview' })).toBeNull()
  })
})

describe('SandboxDetail tabs and fork', () => {
  it('exposes its sections as tabs and marks the open one', async () => {
    renderPage()
    const tabs = await screen.findAllByRole('tab')
    expect(tabs.map(t => t.textContent)).toContain('Files')
    expect(tabs.filter(t => t.getAttribute('aria-selected') === 'true')).toHaveLength(1)
    expect(screen.getByRole('tablist')).toBeTruthy()
    await userEvent.click(screen.getByRole('tab', { name: 'Files' }))
    expect(screen.getByRole('tab', { name: 'Files' }).getAttribute('aria-selected')).toBe('true')
  })

  it('names a fork after the sandbox it came from', async () => {
    const fork = vi.spyOn(api, 'forkSandbox').mockResolvedValue({ id: 'sb2' } as never)
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'More actions' }))
    await userEvent.click(screen.getByRole('button', { name: 'Fork' }))
    await waitFor(() => expect(fork).toHaveBeenCalledWith('sb1', 'my-app-fork'))
  })
})

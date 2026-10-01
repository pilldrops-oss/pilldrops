import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import SandboxDetail, { InfoTab } from '../../src/pages/SandboxDetail'
import { DeployProgress } from '../../src/components/launch/DeployProgress'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'
import type { SandboxDetail as SB } from '../../src/types'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  getSandbox: vi.fn(),
  listTemplates: vi.fn(),
  getSandboxMetrics: vi.fn(),
  getPreviewToken: vi.fn(),
  setSandboxVisibility: vi.fn(),
}))
// The terminal itself (xterm + a socket) is not what is under test.
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
})

function renderPage(state?: unknown) {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <MemoryRouter initialEntries={[{ pathname: '/sandbox/sb1', state }]}>
          <Routes><Route path="/sandbox/:id" element={<SandboxDetail />} /></Routes>
        </MemoryRouter>
      </ConfirmProvider>
    </ToastProvider>,
  )
}

describe('sandbox detail (U4)', () => {
  it('Info shows name, storage cap without usage, ports, and copyable preview URL', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    render(<ToastProvider><InfoTab sandbox={{ ...sb, boot_type: 'cold', boot_ms: 1136 }} previewPort={3000} /></ToastProvider>)
    expect(screen.getByText('my-app')).toBeInTheDocument()
    expect(screen.getByText('2 GB (usage not reported)')).toBeInTheDocument()
    expect(screen.getByText('cold')).toBeInTheDocument()
    expect(screen.getByText('1136 ms')).toBeInTheDocument()
    expect(screen.getByText(/3000 preview · 7681 terminal/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Copy Preview URL' }))
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/preview/sb1/3000/`)
  })

  it('heads the page with the sandbox name and keeps the create response boot fields', async () => {
    renderPage({ sandbox: { ...sb, boot_type: 'snapshot', boot_ms: 412 } })
    expect(await screen.findByText('my-app', { selector: '.detail-name' })).toBeInTheDocument()
    await waitFor(() => expect(api.getSandbox).toHaveBeenCalled())
    await userEvent.click(screen.getByRole('button', { name: 'Info' }))
    expect(await screen.findByText('412 ms')).toBeInTheDocument()
    expect(screen.getByText('snapshot')).toBeInTheDocument()
  })

  it('keeps the same terminal across a tab switch', async () => {
    renderPage()
    const pty = await screen.findByTestId('pty')
    await userEvent.click(screen.getByRole('button', { name: 'Files' }))
    await userEvent.click(screen.getByRole('button', { name: 'Terminal' }))
    expect(screen.getByTestId('pty')).toBe(pty)
  })

  it('names the API-access control for what it does', async () => {
    vi.mocked(api.setSandboxVisibility).mockResolvedValue({} as never)
    renderPage()
    const access = await screen.findByRole('combobox', { name: /API access/ })
    await userEvent.selectOptions(access, 'private')
    await waitFor(() => expect(api.setSandboxVisibility).toHaveBeenCalledWith('sb1', 'private'))
  })

  it('the ready screen builds the preview URL the dashboard way, not from the node domain', () => {
    render(
      <DeployProgress
        steps={[]}
        state="done"
        onReset={() => {}}
        result={{
          id: 'sb1', template: 'node', status: 'running', created_at: '', expires_at: '',
          app_url: 'https://8000-sb1.localtest.me/',
        }}
      />,
    )
    expect(screen.getByText(`${window.location.origin}/preview/sb1/8000/`)).toBeInTheDocument()
    expect(screen.queryByText(/localtest\.me/)).toBeNull()
  })
})

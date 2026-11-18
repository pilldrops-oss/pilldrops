import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import SandboxDetail from '../../src/pages/SandboxDetail'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'
import { ApiError } from '../../src/api'
import type { SandboxDetail as SB, Connector } from '../../src/types'
import { useOrgRole } from '../../src/hooks/useOrgRole'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  getSandbox: vi.fn(),
  listTemplates: vi.fn(),
  getSandboxMetrics: vi.fn(),
  getPreviewToken: vi.fn(),
  listPrivateGrants: vi.fn(),
  grantPrivate: vi.fn(),
  revokePrivate: vi.fn(),
  listConnectors: vi.fn(),
}))
vi.mock('../../src/components/SandboxTerminal', () => ({ default: () => <div data-testid="pty" /> }))
vi.mock('../../src/hooks/useOrgRole', () => ({ useOrgRole: vi.fn(() => 'owner') }))

const original = window.__SANDBOXD__
const sb: SB = {
  id: 'sb1', template: 'node', name: 'my-app', status: 'running', boot_type: '', boot_ms: 0,
  created_at: '2026-10-01T00:00:00Z', expires_at: '2026-10-02T00:00:00Z',
  storage_mb: 2048, url: '/preview/sb1/3000/', visibility: 'org', public_preview: false,
}
const connector: Connector = {
  id: 'cn_1', name: 'prod', created_at: 1, last_seen_at: 2, instances: 1, online: true,
  targets: [{ name: 'db', port: 5432 }],
}
const grant = { connector: 'prod', target: 'db', created_at: 1, online: true, names: ['db.private'] }

beforeEach(() => {
  window.__SANDBOXD__ = { key: '' } as typeof window.__SANDBOXD__
  vi.mocked(api.getSandbox).mockResolvedValue(sb)
  vi.mocked(api.listTemplates).mockResolvedValue([])
  vi.mocked(api.getSandboxMetrics).mockResolvedValue(null as never)
  vi.mocked(api.getPreviewToken).mockResolvedValue('tok')
  vi.mocked(api.listPrivateGrants).mockResolvedValue({ grants: [grant], supported: true })
  vi.mocked(api.grantPrivate).mockResolvedValue(grant)
  vi.mocked(api.revokePrivate).mockResolvedValue(undefined as never)
  vi.mocked(api.listConnectors).mockResolvedValue([connector])
  vi.mocked(useOrgRole).mockReturnValue('owner')
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

describe('sandbox detail private access', () => {
  it('lists a grant with its private name', async () => {
    renderPage()
    expect(await screen.findByText('db.private')).toBeInTheDocument()
  })

  it('adds a grant by picking a connector then a target', async () => {
    renderPage()
    await userEvent.selectOptions(await screen.findByRole('combobox', { name: 'Connector' }), 'prod')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Target' }), 'db')
    await userEvent.click(screen.getByRole('button', { name: 'Add' }))
    await waitFor(() => expect(api.grantPrivate).toHaveBeenCalledWith('sb1', 'prod', 'db'))
  })

  it('asks before removing a grant', async () => {
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove db' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(api.revokePrivate).toHaveBeenCalledWith('sb1', 'prod', 'db'))
  })

  it('explains an unsupported node and disables Add', async () => {
    vi.mocked(api.listPrivateGrants).mockResolvedValue({ grants: [], supported: false })
    renderPage()
    expect(await screen.findByText("Private access is not available on this sandbox's node.")).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Add' })).toBeDisabled()
  })

  it('shows a member the list without Add or Remove', async () => {
    vi.mocked(useOrgRole).mockReturnValue('member')
    renderPage()
    await screen.findByText('db.private')
    expect(screen.queryByRole('button', { name: 'Add' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Remove db' })).not.toBeInTheDocument()
  })

  it('shows the server message when a name is taken', async () => {
    vi.mocked(api.grantPrivate).mockRejectedValue(
      new ApiError({ status: 409, code: 'private_name_taken', method: 'POST', path: '/v1/sandboxes/sb1/private', message: 'db.private is already granted from another connector' }),
    )
    renderPage()
    await userEvent.selectOptions(await screen.findByRole('combobox', { name: 'Connector' }), 'prod')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Target' }), 'db')
    await userEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText(/already granted from another connector/)).toBeInTheDocument()
  })

  it('hides itself when the server has no private ingress', async () => {
    vi.mocked(api.listPrivateGrants).mockRejectedValue(
      new ApiError({ status: 501, code: 'no_private_ingress', method: 'GET', path: '/v1/sandboxes/sb1/private', message: 'off' }),
    )
    renderPage()
    await waitFor(() => expect(api.listPrivateGrants).toHaveBeenCalled())
    await screen.findByText('my-app')
    expect(screen.queryByText('Private access')).not.toBeInTheDocument()
  })

  it('points to Connectors when there are none', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([])
    renderPage()
    expect(await screen.findByText(/No connectors yet/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Create one' })).toHaveAttribute('href', '/connectors')
  })

  it('toasts a failed connector fetch', async () => {
    vi.mocked(api.listConnectors).mockRejectedValue(new Error('connectors down'))
    renderPage()
    expect(await screen.findByText(/connectors down/)).toBeInTheDocument()
  })

  it('keeps the grant when removal is cancelled', async () => {
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove db' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    expect(api.revokePrivate).not.toHaveBeenCalled()
  })
})

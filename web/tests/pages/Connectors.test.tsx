import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import * as api from '../../src/api'
import Connectors from '../../src/pages/Connectors'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import type { Connector } from '../../src/types'

vi.mock('../../src/api', () => ({
  listConnectors: vi.fn(), createConnector: vi.fn(), revokeConnector: vi.fn(),
  listConnectorKeys: vi.fn(), createConnectorKey: vi.fn(), revokeConnectorKey: vi.fn(),
  listConnectorConnections: vi.fn(), getCapabilities: vi.fn(),
}))
vi.mock('../../src/hooks/useOrgRole', () => ({ useOrgRole: vi.fn(() => 'owner') }))
import { useOrgRole } from '../../src/hooks/useOrgRole'

const conn = (over: Partial<Connector> = {}): Connector => ({
  id: 'cn_1', name: 'prod', created_at: 1, last_seen_at: 2, instances: 1, online: true,
  targets: [{ name: 'db', port: 5432 }, { name: 'api', port: 443, host: 'internal.corp' }], ...over,
})
const renderPage = () => render(<ToastProvider><ConfirmProvider><Connectors /></ConfirmProvider></ToastProvider>)

beforeEach(() => {
  vi.mocked(api.listConnectors).mockReset().mockResolvedValue([])
  vi.mocked(api.createConnector).mockReset().mockResolvedValue(conn({ online: false, instances: 0, targets: [] }))
  vi.mocked(api.revokeConnector).mockReset().mockResolvedValue(undefined as never)
  vi.mocked(api.listConnectorKeys).mockReset().mockResolvedValue([])
  vi.mocked(api.createConnectorKey).mockReset().mockResolvedValue({ id: 'ck_1', key: `pd_conn_${  'a'.repeat(48)}`, created_at: 1, last_used_at: 0 })
  vi.mocked(api.listConnectorConnections).mockReset().mockResolvedValue([])
  vi.mocked(api.getCapabilities).mockReset().mockResolvedValue({ private_access: true } as never)
  vi.mocked(useOrgRole).mockReturnValue('owner')
})

describe('Connectors', () => {
  it('shows the empty state', async () => {
    renderPage()
    expect(await screen.findByText('No connectors yet.')).toBeInTheDocument()
  })

  it('lists targets and online state', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    const row = (await screen.findByText('prod')).closest('tr')!
    expect(within(row).getByText('online · 1 instance')).toBeInTheDocument()
    expect(within(row).getByText('db:5432')).toBeInTheDocument()
    expect(within(row).getByText('api:443 (internal.corp)')).toBeInTheDocument()
  })

  it('creates a connector and shows the run command', async () => {
    renderPage()
    await userEvent.type(await screen.findByLabelText('Connector name'), 'prod')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))
    expect(api.createConnector).toHaveBeenCalledWith('prod')
    expect(await screen.findByText(/pilldrops connector run prod db=HOST:PORT/)).toBeInTheDocument()
  })

  it('shows a new key once', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Keys for prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Create key' }))
    expect(await screen.findByText(`pd_conn_${  'a'.repeat(48)}`)).toBeInTheDocument()
    expect(screen.getByText(/shown once/)).toBeInTheDocument()
  })

  it('hides changes from a member', async () => {
    vi.mocked(useOrgRole).mockReturnValue('member')
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await screen.findByText('prod')
    expect(screen.queryByLabelText('Connector name')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Remove prod' })).not.toBeInTheDocument()
  })

  it('says when the server has no private access', async () => {
    vi.mocked(api.getCapabilities).mockResolvedValue({ private_access: false } as never)
    renderPage()
    expect(await screen.findByText(/This server does not offer private access/)).toBeInTheDocument()
  })

  it('shows a load error', async () => {
    vi.mocked(api.listConnectors).mockRejectedValue(new Error('boom'))
    renderPage()
    expect(await screen.findByText(/boom/)).toBeInTheDocument()
  })
})

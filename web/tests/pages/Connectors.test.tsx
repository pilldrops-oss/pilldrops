import { render, screen, waitFor, within } from '@testing-library/react'
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
  listTagGrants: vi.fn(), grantTag: vi.fn(), revokeTagGrant: vi.fn(),
}))
vi.mock('../../src/hooks/useOrgRole', () => ({ useOrgRole: vi.fn(() => 'owner') }))
import { useOrgRole } from '../../src/hooks/useOrgRole'

const conn = (over: Partial<Connector> = {}): Connector => ({
  id: 'cn_1', name: 'prod', created_at: 1, last_seen_at: 2, instances: 1, online: true,
  targets: [{ name: 'db', port: 5432 }, { name: 'api', port: 443, host: 'internal.corp' }],
  subnets: ['10.0.0.0/16'], dns: { suffixes: ['corp.internal'] }, ...over,
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
  vi.mocked(api.listTagGrants).mockReset().mockResolvedValue([])
  vi.mocked(api.grantTag).mockReset().mockResolvedValue({ id: 'pg_9', tag: 'backend', connector: 'prod', kind: 'subnet', value: '10.0.5.0/24', created_at: 1, online: true, sandboxes: 2, unsupported: [] })
  vi.mocked(api.revokeTagGrant).mockReset().mockResolvedValue(undefined as never)
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
    const row = (await screen.findByRole('cell', { name: 'prod' })).closest('tr')!
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

  it('says when the server has no private access, and does not poll', async () => {
    vi.mocked(api.getCapabilities).mockResolvedValue({ private_access: false } as never)
    vi.mocked(api.listConnectors).mockClear()
    renderPage()
    expect(await screen.findByText(/This server does not offer private access/)).toBeInTheDocument()
    expect(api.listConnectors).not.toHaveBeenCalled()
  })

  it('shows a load error', async () => {
    vi.mocked(api.listConnectors).mockRejectedValue(new Error('boom'))
    renderPage()
    expect(await screen.findByText(/boom/)).toBeInTheDocument()
  })

  it('does not leak a new key into another connector', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn(), conn({ id: 'cn_2', name: 'stage' })])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Keys for prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Create key' }))
    const key = `pd_conn_${'a'.repeat(48)}`
    expect(await screen.findByText(key)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Keys for stage' }))
    expect(screen.queryByText(key)).not.toBeInTheDocument()
  })

  it('Done hides the key and closes the panel', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Keys for prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Create key' }))
    await screen.findByText(/shown once/)
    await userEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByText(/shown once/)).not.toBeInTheDocument()
  })

  it('asks before removing a connector', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    expect(api.revokeConnector).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Remove prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(api.revokeConnector).toHaveBeenCalledWith('cn_1'))
  })

  it('closes the open panel when its connector is removed', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Keys for prod' }))
    expect(await screen.findByRole('button', { name: 'Create key' })).toBeInTheDocument()
    vi.mocked(api.listConnectors).mockResolvedValue([])
    await userEvent.click(screen.getByRole('button', { name: 'Remove prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Create key' })).not.toBeInTheDocument())
    expect(await screen.findByText('No connectors yet.')).toBeInTheDocument()
  })

  it('asks before revoking a key', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    vi.mocked(api.listConnectorKeys).mockResolvedValue([{ id: 'ck_9', created_at: 1, last_used_at: 0 }])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Keys for prod' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Revoke' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    expect(api.revokeConnectorKey).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Revoke' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(api.revokeConnectorKey).toHaveBeenCalledWith('cn_1', 'ck_9'))
  })

  it('hides Keys from a member', async () => {
    vi.mocked(useOrgRole).mockReturnValue('member')
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await screen.findByText('prod')
    expect(screen.queryByRole('button', { name: 'Keys for prod' })).not.toBeInTheDocument()
  })

  it('shows the connection log and its empty state', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Log for prod' }))
    expect(await screen.findByText('No connections yet.')).toBeInTheDocument()
    vi.mocked(api.listConnectorConnections).mockResolvedValue([
      { id: 'c1', sandbox_id: 'sb9', target: 'db', started_at: 10, ended_at: 15, bytes_in: 3, bytes_out: 4, result: 'dial_failed' },
    ])
    await userEvent.click(screen.getByRole('button', { name: 'Done' }))
    await userEvent.click(screen.getByRole('button', { name: 'Log for prod' }))
    expect(await screen.findByText('sb9')).toBeInTheDocument()
    expect(screen.getByText('dial failed')).toBeInTheDocument()
    expect(screen.getByText('5s')).toBeInTheDocument()
  })

  it('lists subnets and dns suffixes beside targets', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    const row = (await screen.findByRole('cell', { name: 'prod' })).closest('tr')!
    expect(within(row).getByText('10.0.0.0/16')).toBeInTheDocument()
    expect(within(row).getByText('*.corp.internal')).toBeInTheDocument()
  })

  it('grants a subnet to a tag', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    renderPage()
    await userEvent.type(await screen.findByRole('textbox', { name: 'Tag' }), 'backend')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Tag connector' }), 'prod')
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Tag kind' }), 'subnet')
    await userEvent.type(screen.getByRole('textbox', { name: 'Tag value' }), '10.0.5.0/24')
    await userEvent.click(screen.getByRole('button', { name: 'Grant' }))
    await waitFor(() => expect(api.grantTag).toHaveBeenCalledWith('backend', 'prod', 'subnet', '10.0.5.0/24'))
  })

  it('removes a tag grant after confirming', async () => {
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    vi.mocked(api.listTagGrants).mockResolvedValue([{ id: 'pg_9', tag: 'backend', connector: 'prod', kind: 'dns', value: 'corp.internal', created_at: 1, online: true }])
    renderPage()
    await userEvent.click(await screen.findByRole('button', { name: 'Remove corp.internal from backend' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(api.revokeTagGrant).toHaveBeenCalledWith('pg_9'))
  })

  it('shows tag grants to a member without the form', async () => {
    vi.mocked(useOrgRole).mockReturnValue('member')
    vi.mocked(api.listConnectors).mockResolvedValue([conn()])
    vi.mocked(api.listTagGrants).mockResolvedValue([{ id: 'pg_9', tag: 'backend', connector: 'prod', kind: 'dns', value: 'corp.internal', created_at: 1, online: true }])
    renderPage()
    expect(await screen.findByText('backend')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Grant' })).not.toBeInTheDocument()
  })
})

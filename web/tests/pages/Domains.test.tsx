import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Domains from '../../src/pages/Domains'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'
import type { CustomDomain } from '../../src/types'

vi.mock('../../src/api', () => ({
  listDomains: vi.fn(),
  addDomain: vi.fn(),
  removeDomain: vi.fn(),
}))

const row = (over: Partial<CustomDomain> = {}): CustomDomain => ({
  host: 'app.example.com',
  sandbox_id: 'sb1',
  port: 8000,
  wildcard: false,
  status: 'pending',
  error: '',
  record: 'app.example.com CNAME 8000-sb1.pilldrops.com',
  cname_target: '8000-sb1.pilldrops.com',
  created_at: 1,
  ...over,
})

function renderDomains() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <Domains />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.mocked(api.listDomains).mockReset().mockResolvedValue([])
  vi.mocked(api.addDomain).mockReset().mockResolvedValue(row())
  vi.mocked(api.removeDomain).mockReset().mockResolvedValue(undefined as never)
})

describe('Domains', () => {
  it('adds a host, shows the record to create, and reads pending until the verifier flips it', async () => {
    const user = userEvent.setup()
    renderDomains()

    await user.type(await screen.findByLabelText('host'), 'app.example.com')
    await user.type(screen.getByLabelText('sandbox'), 'sb1')
    vi.mocked(api.listDomains).mockResolvedValue([row()])
    await user.click(screen.getByRole('button', { name: 'Add domain' }))

    await waitFor(() => expect(api.addDomain).toHaveBeenCalledWith('app.example.com', 'sb1', undefined))
    expect(await screen.findByText('pending')).toBeInTheDocument()
    expect(screen.getByText('app.example.com CNAME 8000-sb1.pilldrops.com')).toBeInTheDocument()

    // What the verifier writes shows up on the next read of the list.
    vi.mocked(api.listDomains).mockResolvedValue([row({ status: 'active' })])
    await user.type(screen.getByLabelText('host'), 'second.example.com')
    await user.click(screen.getByRole('button', { name: 'Add domain' }))
    expect(await screen.findByText('active')).toBeInTheDocument()
  })

  it('shows the resolver reason a host is still pending', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([
      row({ error: 'app.example.com points at 203.0.113.1, not at 8000-sb1.pilldrops.com' }),
    ])
    renderDomains()

    expect(await screen.findByText(/points at 203\.0\.113\.1/)).toBeInTheDocument()
  })

  it('asks before removing a host', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([row({ status: 'active' })])
    const user = userEvent.setup()
    renderDomains()

    await user.click(await screen.findByRole('button', { name: 'Remove' }))
    await user.click(await screen.findByRole('button', { name: 'Confirm' }))

    await waitFor(() => expect(api.removeDomain).toHaveBeenCalledWith('app.example.com'))
  })
})

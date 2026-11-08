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
  getCapabilities: vi.fn(),
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
  vi.mocked(api.getCapabilities).mockReset().mockResolvedValue({ custom_domains: true })
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

  // B25: show the 501 reason up front, and disable the form rather than let
  // the user fill it in only to have the add fail.
  it('shows the feature-off reason up front and disables the form', async () => {
    vi.mocked(api.getCapabilities).mockResolvedValue({ custom_domains: false })
    renderDomains()

    expect(await screen.findByText(/custom domains are off on this server/i)).toBeInTheDocument()
    expect(screen.getByLabelText('host')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Add domain' })).toBeDisabled()
    expect(api.addDomain).not.toHaveBeenCalled()
  })

  // B25: a bad hostname is rejected on the client, not sent to the server.
  it('validates the hostname before submitting', async () => {
    const user = userEvent.setup()
    renderDomains()

    await user.type(await screen.findByLabelText('host'), 'not a host!')
    await user.click(screen.getByRole('button', { name: 'Add domain' }))

    expect(await screen.findByText(/at least two labels/i)).toBeInTheDocument()
    expect(api.addDomain).not.toHaveBeenCalled()
  })

  it('asks the owner to update a host still on the old domain', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([
      row({ status: 'active', legacy_target: true, record: 'app.example.com CNAME 8000-sb1.pilldrops.app' }),
    ])
    renderDomains()

    expect(await screen.findByText(/Update your DNS record to/)).toBeInTheDocument()
  })

  it('shows the TXT record and the deadline for a host in its grace period', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([
      row({
        status: 'active',
        txt_record: '_pilldrops-challenge.app.example.com TXT abc123',
        txt_required: true,
        txt_grace_ends_at: 1_000_000_000,
      }),
    ])
    renderDomains()

    expect(await screen.findByText('_pilldrops-challenge.app.example.com TXT abc123')).toBeInTheDocument()
    expect(screen.getByText(new RegExp(new Date(1_000_000_000_000).toLocaleDateString()))).toBeInTheDocument()
  })

  it('shows the TXT record a pending host needs', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([
      row({ txt_record: '_pilldrops-challenge.app.example.com TXT abc123', txt_required: true }),
    ])
    renderDomains()

    expect(await screen.findByText('_pilldrops-challenge.app.example.com TXT abc123')).toBeInTheDocument()
    expect(screen.getByText(/to prove it is yours/)).toBeInTheDocument()
    expect(screen.queryByText(/keeps working until/)).not.toBeInTheDocument()
  })

  it('offers the TXT record as an alternative without warning when it is not required', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([
      row({ status: 'active', txt_record: '_pilldrops-challenge.app.example.com TXT abc123', txt_required: false }),
    ])
    renderDomains()

    expect(await screen.findByText('_pilldrops-challenge.app.example.com TXT abc123')).toBeInTheDocument()
    expect(screen.queryByText(/to prove it is yours/)).not.toBeInTheDocument()
  })

  it('says nothing about DNS for a host on the current domain', async () => {
    vi.mocked(api.listDomains).mockResolvedValue([row({ status: 'active' })])
    renderDomains()

    expect(await screen.findByText('active')).toBeInTheDocument()
    expect(screen.queryByText(/Update your DNS record/)).not.toBeInTheDocument()
    expect(screen.queryByText(/_pilldrops-challenge/)).not.toBeInTheDocument()
  })
})

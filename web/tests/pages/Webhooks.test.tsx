import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import Webhooks from '../../src/pages/Webhooks'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'
import type { Webhook } from '../../src/types'

vi.mock('../../src/api', () => ({
  listWebhooks: vi.fn(),
  listWebhookDeliveries: vi.fn(),
  addWebhook: vi.fn(),
  deleteWebhook: vi.fn(),
  testWebhook: vi.fn(),
  setWebhookEnabled: vi.fn(),
}))

const hook = (over: Partial<Webhook> = {}): Webhook => ({
  id: 'wh_1',
  url: 'https://hooks.example.com/in',
  events: ['sandbox.ended'],
  enabled: true,
  consecutive_failures: 0,
  last_at: '0001-01-01T00:00:00Z',
  ...over,
})

function mount(hooks: Webhook[] = []) {
  vi.mocked(api.listWebhooks).mockResolvedValue({
    webhooks: hooks,
    deliverable_events: ['sandbox.created', 'sandbox.ended', 'sandbox.crashed'],
  })
  return render(
    <MemoryRouter>
      <ConfirmProvider>
        <Webhooks />
      </ConfirmProvider>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  vi.resetAllMocks()
  vi.mocked(api.listWebhookDeliveries).mockResolvedValue({ deliveries: null })
})

describe('Webhooks', () => {
  it('adds a webhook and shows its secret once', async () => {
    vi.mocked(api.addWebhook).mockResolvedValue({ webhook: hook(), secret: 'whsec_0123456789abcdefghij' })
    mount()
    await userEvent.type(await screen.findByPlaceholderText(/example.com/), 'https://hooks.example.com/in')
    await userEvent.click(screen.getByText('Add webhook'))

    expect(api.addWebhook).toHaveBeenCalledWith('https://hooks.example.com/in', ['sandbox.created', 'sandbox.ended'])
    expect(await screen.findByText(/shown once/i)).toBeTruthy()
    expect(screen.getByText('whsec_0123456789abcdefghij')).toBeTruthy()
  })

  it('says a hook has no deliveries yet when the server sends a zero time', async () => {
    mount([hook()])
    expect(await screen.findByText('No deliveries yet')).toBeTruthy()
  })

  it('reports what the endpoint answered to a test', async () => {
    vi.mocked(api.testWebhook).mockResolvedValue({ ok: false, status: 404, error: 'endpoint answered 404' })
    mount([hook()])
    await userEvent.click(await screen.findByText('Send test'))
    expect(await screen.findByText(/Not delivered. endpoint answered 404/)).toBeTruthy()
  })

  it('deletes only after the confirmation', async () => {
    vi.mocked(api.deleteWebhook).mockResolvedValue({ deleted: 'wh_1' })
    mount([hook()])
    await userEvent.click(await screen.findByText('Delete'))
    expect(api.deleteWebhook).not.toHaveBeenCalled()
    await userEvent.click(screen.getAllByText('Delete').slice(-1)[0])
    await waitFor(() => expect(api.deleteWebhook).toHaveBeenCalledWith('wh_1'))
  })

  it('pauses a hook, lists recent deliveries and offers the curl for the org route', async () => {
    vi.mocked(api.listWebhookDeliveries).mockResolvedValue({
      deliveries: [{
        id: 'whd_1', webhook_id: 'wh_1', sandbox_id: 'sb_9', event_type: 'sandbox.ended',
        state: 'sent', attempts: 1, last_status: 200, created_at: new Date().toISOString(),
      }],
    })
    vi.mocked(api.setWebhookEnabled).mockResolvedValue({ id: 'wh_1', enabled: false })
    mount([hook()])

    expect(await screen.findByText(/curl -X POST .*\/v1\/webhooks /)).toBeTruthy()
    expect(screen.getByText('sb_9')).toBeTruthy()
    expect(screen.getByText('answered 200')).toBeTruthy()

    await userEvent.click(screen.getByText('Pause'))
    expect(api.setWebhookEnabled).toHaveBeenCalledWith('wh_1', false)
  })
})

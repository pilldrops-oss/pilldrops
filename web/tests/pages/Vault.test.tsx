import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Vault from '../../src/pages/Vault'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'

vi.mock('../../src/api', () => ({
  listSecrets: vi.fn(),
  putSecret: vi.fn(),
  deleteSecret: vi.fn(),
}))

const secret = (name: string, last4 = 'abcd', value?: string) =>
  ({ name, last4, value, created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z' })

function renderVault() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <Vault />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.mocked(api.listSecrets).mockReset().mockResolvedValue([])
  vi.mocked(api.putSecret).mockReset().mockResolvedValue(undefined as never)
  vi.mocked(api.deleteSecret).mockReset().mockResolvedValue(undefined as never)
})

describe('Vault — provider keys', () => {
  it('connects a provider key', async () => {
    const user = userEvent.setup()
    renderVault()

    const field = await screen.findByLabelText('Anthropic API key')
    await user.type(field, 'sk-ant-test')
    await user.click(screen.getAllByRole('button', { name: 'Connect' })[0])

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('ANTHROPIC_API_KEY', 'sk-ant-test'))
  })

  it('replaces a key without disconnecting first', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([secret('ANTHROPIC_API_KEY', 'wxyz')])
    const user = userEvent.setup()
    renderVault()

    expect(await screen.findByText(/connected · …wxyz/)).toBeInTheDocument()
    await user.click(screen.getAllByRole('button', { name: 'Replace' })[0])
    await user.type(screen.getByLabelText('Anthropic API key'), 'sk-ant-new')
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0])

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('ANTHROPIC_API_KEY', 'sk-ant-new'))
    expect(api.deleteSecret).not.toHaveBeenCalled()
  })

  it('asks before disconnecting, and does nothing if cancelled', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([secret('ANTHROPIC_API_KEY')])
    const user = userEvent.setup()
    renderVault()

    await user.click(await screen.findByRole('button', { name: 'Disconnect' }))
    await user.click(await screen.findByRole('button', { name: /cancel/i }))
    expect(api.deleteSecret).not.toHaveBeenCalled()
  })
})

describe('Vault — gateway', () => {
  it('saves a custom base URL for Anthropic', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Anthropic API key')
    await user.click(screen.getByText(/Using a gateway or proxy instead of https:\/\/api\.anthropic\.com/))

    const url = screen.getByLabelText(/ANTHROPIC_BASE_URL/)
    await user.type(url, 'https://gw.corp.example')
    await user.click(screen.getAllByRole('button', { name: 'Save gateway' })[0])

    await waitFor(() =>
      expect(api.putSecret).toHaveBeenCalledWith('ANTHROPIC_BASE_URL', 'https://gw.corp.example'))
  })

  it('strips a pasted /v1 rather than saving a doubled path', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Anthropic API key')
    await user.click(screen.getByText(/Using a gateway or proxy instead of https:\/\/api\.anthropic\.com/))
    await user.type(screen.getByLabelText(/ANTHROPIC_BASE_URL/), 'https://litellm.internal/v1')
    await user.click(screen.getAllByRole('button', { name: 'Save gateway' })[0])

    await waitFor(() =>
      expect(api.putSecret).toHaveBeenCalledWith('ANTHROPIC_BASE_URL', 'https://litellm.internal'))
  })

  it('refuses a URL with no scheme and does not call the API', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Anthropic API key')
    await user.click(screen.getByText(/Using a gateway or proxy instead of https:\/\/api\.anthropic\.com/))
    await user.type(screen.getByLabelText(/ANTHROPIC_BASE_URL/), 'gw.corp.example')
    await user.click(screen.getAllByRole('button', { name: 'Save gateway' })[0])

    expect(await screen.findByText(/include the scheme/i)).toBeInTheDocument()
    expect(api.putSecret).not.toHaveBeenCalled()
  })

  it('shows the configured gateway so it can be verified', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([
      secret('ANTHROPIC_API_KEY'),
      secret('ANTHROPIC_BASE_URL', 'mple', 'https://gw.corp.example'),
    ])
    renderVault()

    expect(await screen.findByText('https://gw.corp.example')).toBeInTheDocument()
    expect(screen.getByLabelText(/ANTHROPIC_BASE_URL/)).toHaveValue('https://gw.corp.example')
  })

  it('offers a gateway for every provider the platform routes', async () => {
    // Anthropic, OpenAI and xAI; Amp reads its endpoint from a settings file instead.
    renderVault()
    await screen.findByLabelText('Anthropic API key')
    expect(screen.getAllByText(/Using a gateway or proxy/)).toHaveLength(3)
    expect(screen.getByLabelText(/GROK_BASE_URL/)).toBeInTheDocument()
  })

  it('explains why Amp has no gateway field rather than leaving a gap', async () => {
    renderVault()
    await screen.findByLabelText('Anthropic API key')
    expect(screen.getByText(/Amp reads its endpoint from its settings file/i)).toBeInTheDocument()
  })

  it('stores a gateway URL for xAI under its own env var', async () => {
    const user = userEvent.setup()
    renderVault()
    await screen.findByLabelText('Anthropic API key')
    await user.click(screen.getByText(/Using a gateway or proxy instead of https:\/\/api\.x\.ai/))
    await user.type(screen.getByLabelText(/GROK_BASE_URL/), 'https://xai-gw.corp.example')
    await user.click(screen.getAllByRole('button', { name: 'Save gateway' })[2])
    await waitFor(() =>
      expect(api.putSecret).toHaveBeenCalledWith('GROK_BASE_URL', 'https://xai-gw.corp.example'))
  })
})

describe('Vault — custom secrets', () => {
  it('rejects a name that is not a valid env identifier', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Secret name')
    // Lower case is upper-cased as you type; a dash is what actually breaks it.
    await user.type(screen.getByLabelText('Secret name'), 'MY-SECRET')
    await user.type(screen.getByLabelText('Secret value'), 'value')

    expect(await screen.findByText(/ENV_VAR-style name/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Add secret' })).toBeDisabled()
    expect(api.putSecret).not.toHaveBeenCalled()
  })

  it('adds a valid custom secret', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Secret name')
    await user.type(screen.getByLabelText('Secret name'), 'DATABASE_URL')
    await user.type(screen.getByLabelText('Secret value'), 'postgres://x')
    await user.click(screen.getByRole('button', { name: 'Add secret' }))

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('DATABASE_URL', 'postgres://x'))
  })

  it('keeps gateway settings out of the custom list', async () => {
    // Gateway settings render in their provider row; listing them twice invites a half-deletion.
    vi.mocked(api.listSecrets).mockResolvedValue([
      secret('ANTHROPIC_BASE_URL', 'mple', 'https://gw.corp.example'),
      secret('OPENAI_MODEL', 'mini', 'gpt-4o-mini'),
      secret('DATABASE_URL'),
    ])
    renderVault()

    const custom = (await screen.findByText('Custom secrets')).closest('.panel')!
    expect(within(custom as HTMLElement).getByText('DATABASE_URL')).toBeInTheDocument()
    expect(within(custom as HTMLElement).queryByText('ANTHROPIC_BASE_URL')).toBeNull()
    expect(within(custom as HTMLElement).queryByText('OPENAI_MODEL')).toBeNull()
  })

  it('reports a failed save instead of silently doing nothing', async () => {
    vi.mocked(api.putSecret).mockRejectedValue(new Error('vault is locked'))
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Secret name')
    await user.type(screen.getByLabelText('Secret name'), 'TOKEN')
    await user.type(screen.getByLabelText('Secret value'), 'x')
    await user.click(screen.getByRole('button', { name: 'Add secret' }))

    expect(await screen.findByText(/vault is locked/)).toBeInTheDocument()
  })
})

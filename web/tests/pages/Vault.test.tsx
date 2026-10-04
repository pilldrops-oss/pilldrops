import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Vault from '../../src/pages/Vault'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'

vi.mock('../../src/api', () => ({
  listSecrets: vi.fn(),
  putSecret: vi.fn(),
  deleteSecret: vi.fn(),
  setSecretAutoInject: vi.fn(),
}))

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
  vi.mocked(api.setSecretAutoInject).mockReset().mockResolvedValue(undefined as never)
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

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('DATABASE_URL', 'postgres://x', false))
  })

  it('replaces the value of an existing secret under the same name, without touching auto-inject', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([
      { name: 'AAA', kind: 'secret', auto_inject: false, last4: 'sdad', created_at: '2026-10-01T00:00:00Z' },
    ] as never)
    const user = userEvent.setup()
    renderVault()

    await user.click(await screen.findByRole('button', { name: 'Replace value' }))
    expect(screen.getByLabelText('Secret name')).toHaveValue('AAA')
    expect(screen.getByLabelText('Secret value')).toHaveFocus()
    // The auto-inject field is now its own per-row toggle, not part of this form.
    expect(screen.queryByLabelText('Inject into every new sandbox')).toBeNull()
    await user.type(screen.getByLabelText('Secret value'), 'new-value')
    await user.click(screen.getByRole('button', { name: 'Replace secret' }))

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('AAA', 'new-value', undefined))
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

  it('toggles an existing secret’s auto-inject flag on its own, without the value', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([
      { name: 'OPENAI_KEY', auto_inject: true, last4: 'abcd', created_at: '', updated_at: '' },
    ])
    const user = userEvent.setup()
    renderVault()

    const toggle = await screen.findByLabelText('Auto-inject OPENAI_KEY')
    expect(toggle).toBeChecked()
    await user.click(toggle)

    await waitFor(() => expect(api.setSecretAutoInject).toHaveBeenCalledWith('OPENAI_KEY', false))
    expect(api.putSecret).not.toHaveBeenCalled()
  })

  it('sends auto-inject explicitly when adding a brand-new secret', async () => {
    const user = userEvent.setup()
    renderVault()

    await screen.findByLabelText('Secret name')
    await user.type(screen.getByLabelText('Secret name'), 'NEW_ONE')
    await user.click(screen.getByLabelText('Inject into every new sandbox'))
    await user.type(screen.getByLabelText('Secret value'), 'v')
    await user.click(screen.getByRole('button', { name: 'Add secret' }))
    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('NEW_ONE', 'v', true))
  })
})

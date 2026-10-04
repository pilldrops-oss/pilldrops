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

  it('replaces the value of an existing secret under the same name', async () => {
    vi.mocked(api.listSecrets).mockResolvedValue([
      { name: 'AAA', kind: 'secret', auto_inject: false, last4: 'sdad', created_at: '2026-10-01T00:00:00Z' },
    ] as never)
    const user = userEvent.setup()
    renderVault()

    await user.click(await screen.findByRole('button', { name: 'Replace value' }))
    expect(screen.getByLabelText('Secret name')).toHaveValue('AAA')
    expect(screen.getByLabelText('Secret value')).toHaveFocus()
    await user.type(screen.getByLabelText('Secret value'), 'new-value')
    await user.click(screen.getByRole('button', { name: 'Replace secret' }))

    await waitFor(() => expect(api.putSecret).toHaveBeenCalledWith('AAA', 'new-value'))
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

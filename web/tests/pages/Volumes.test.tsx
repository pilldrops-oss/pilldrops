import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Volumes from '../../src/pages/Volumes'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  listVolumes: vi.fn(),
  createVolume: vi.fn(),
  deleteVolume: vi.fn(),
}))

const renderPage = () => render(<ToastProvider><ConfirmProvider><Volumes /></ConfirmProvider></ToastProvider>)

beforeEach(() => vi.mocked(api.listVolumes).mockResolvedValue([
  { id: 'vol-free', name: 'data', size_mb: 10240, created_at: 1 },
  { id: 'vol-held', name: 'cache', size_mb: 512, attached_to: 'sb-1', created_at: 2 },
]))
afterEach(() => vi.restoreAllMocks())

describe('Volumes page', () => {
  it('lists volumes with size and holder', async () => {
    renderPage()
    expect(await screen.findByText('vol-free')).toBeInTheDocument()
    expect(screen.getByText('10 GB')).toBeInTheDocument()
    expect(screen.getByText('sb-1')).toBeInTheDocument()
  })

  it('creates a volume in GB', async () => {
    vi.mocked(api.createVolume).mockResolvedValue({ id: 'vol-new', name: 'x', size_mb: 20480, created_at: 3 })
    renderPage()
    await userEvent.type(await screen.findByLabelText('Volume name'), 'x')
    const size = screen.getByLabelText('Size in GB')
    await userEvent.clear(size)
    await userEvent.type(size, '20')
    await userEvent.click(screen.getByRole('button', { name: 'Create volume' }))
    expect(api.createVolume).toHaveBeenCalledWith('x', 20480)
  })

  it('deletes only an unattached volume, after confirming', async () => {
    vi.mocked(api.deleteVolume).mockResolvedValue(undefined)
    renderPage()
    const [free, held] = await screen.findAllByRole('button', { name: 'Delete' })
    expect(held).toBeDisabled()
    await userEvent.click(free)
    expect(api.deleteVolume).not.toHaveBeenCalled()
    // The dialog's own Delete is the last one on the page.
    const buttons = await screen.findAllByRole('button', { name: 'Delete' })
    await userEvent.click(buttons[buttons.length - 1])
    expect(api.deleteVolume).toHaveBeenCalledWith('vol-free')
  })
})

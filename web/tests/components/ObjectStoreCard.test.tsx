import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ObjectStoreCard from '../../src/components/ObjectStoreCard'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import { getObjectStore, putObjectStore } from '../../src/api'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  getObjectStore: vi.fn(),
  putObjectStore: vi.fn(),
}))

const savedView = {
  configured: true,
  source: 'saved' as const,
  endpoint: 'https://s3.example.test',
  bucket: 'artifacts',
  region: 'eu-west-3',
  access_key_hint: 'WXYZ',
}

function renderCard() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <ObjectStoreCard />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

describe('ObjectStoreCard', () => {
  it('shows a saved bucket without any key field', async () => {
    vi.mocked(getObjectStore).mockResolvedValue(savedView)
    renderCard()
    expect(await screen.findByText('artifacts')).toBeInTheDocument()
    expect(screen.getByText('WXYZ')).toBeInTheDocument()
    expect(screen.queryByLabelText('Secret key')).toBeNull()
  })

  it('drops the typed keys once they are saved', async () => {
    vi.mocked(getObjectStore).mockResolvedValue({ configured: false })
    vi.mocked(putObjectStore).mockResolvedValue(savedView)
    renderCard()
    const typed = { Endpoint: 'https://s3.example.test', Bucket: 'artifacts', 'Access key': 'access-test', 'Secret key': 'secret-test' }
    for (const [label, value] of Object.entries(typed)) {
      fireEvent.change(await screen.findByLabelText(label), { target: { value } })
    }
    fireEvent.click(screen.getByRole('button', { name: 'Test and save' }))
    expect(await screen.findByText('WXYZ')).toBeInTheDocument()
    expect(putObjectStore).toHaveBeenCalledWith(expect.objectContaining({ secret_key: 'secret-test' }))
    expect(document.body.innerHTML).not.toContain('secret-test')
    fireEvent.click(screen.getByRole('button', { name: 'Replace' }))
    expect(screen.getByLabelText('Secret key')).toHaveValue('')
  })
})

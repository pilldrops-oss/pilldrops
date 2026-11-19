import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import Shell from '../../src/components/Shell'
import { checkAdminAccess, getCapabilities } from '../../src/api'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  checkAdminAccess: vi.fn(),
  getCapabilities: vi.fn(),
}))

function renderShell() {
  vi.mocked(checkAdminAccess).mockResolvedValue('forbidden')
  return render(
    <MemoryRouter initialEntries={['/sandboxes']}>
      <Routes>
        <Route element={<Shell />}>
          <Route path="*" element={null} />
        </Route>
      </Routes>
    </MemoryRouter>,
  )
}

describe('Shell connectors nav', () => {
  it('shows Connectors when the server offers private access', async () => {
    vi.mocked(getCapabilities).mockResolvedValue({ custom_domains: false, private_access: true })
    renderShell()
    expect(await screen.findByRole('link', { name: /Connectors/ })).toBeInTheDocument()
  })

  it('hides it otherwise', async () => {
    vi.mocked(getCapabilities).mockResolvedValue({ custom_domains: false, private_access: false })
    renderShell()
    await screen.findByRole('link', { name: /Vault/ })
    await Promise.resolve()
    expect(screen.queryByRole('link', { name: /Connectors/ })).toBeNull()
  })
})

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import Shell from '../../src/components/Shell'
import { checkAdminAccess } from '../../src/api'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  checkAdminAccess: vi.fn(),
}))

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route element={<Shell />}>
          <Route path="*" element={null} />
        </Route>
      </Routes>
    </MemoryRouter>,
  )
}

describe('Shell admin nav', () => {
  it('shows the admin pages to an operator in the customer console', async () => {
    vi.mocked(checkAdminAccess).mockResolvedValue('ok')
    renderAt('/fleet')
    expect(await screen.findByRole('link', { name: /Fleet/ })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Vault/ })).toBeInTheDocument()
    expect(document.querySelector('.bc b')?.textContent).toBe('Fleet')
  })

  it('keeps them hidden from a member', async () => {
    vi.mocked(checkAdminAccess).mockResolvedValue('forbidden')
    renderAt('/sandboxes')
    await screen.findByRole('link', { name: /Vault/ })
    await Promise.resolve()
    expect(screen.queryByRole('link', { name: /Fleet/ })).toBeNull()
  })
})

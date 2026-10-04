import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import App from '../src/App'

vi.mock('../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../src/api')>()),
  checkAdminAccess: vi.fn().mockResolvedValue('forbidden'),
}))

describe('unknown routes (B15)', () => {
  afterEach(() => {
    window.location.hash = ''
  })

  it('renders a not-found panel instead of a blank page', async () => {
    window.location.hash = '#/does-not-exist'
    render(<App />)
    expect(await screen.findByText('Page not found.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '← Back' })).toBeInTheDocument()
  })
})

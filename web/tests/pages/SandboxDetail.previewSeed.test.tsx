import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { PreviewTab } from '../../src/pages/SandboxDetail'
import { getSandboxPorts } from '../../src/api'

vi.mock('../../src/api', async importOriginal => {
  const actual = await importOriginal<typeof import('../../src/api')>()
  return { ...actual, getSandboxPorts: vi.fn() }
})

const original = window.__SANDBOXD__

// B5: the Screen tab opened before the GET arrived seeded port 8000 and kept
// it, so a desktop previewed the wrong port until a reload.
describe('PreviewTab before the preview port is known', () => {
  beforeEach(() => {
    vi.mocked(getSandboxPorts).mockResolvedValue({ ports: [] })
    window.__SANDBOXD__ = { key: '', sandboxDomain: 'pilldrops.com' } as typeof window.__SANDBOXD__
  })
  afterEach(() => {
    window.__SANDBOXD__ = original
  })

  it('loads nothing, never 8000, then follows the port once it resolves', async () => {
    const { container, rerender } = render(<PreviewTab id="sb1" status="running" />)
    expect(screen.getByText(/finding the preview port/i)).toBeInTheDocument()
    expect(container.querySelector('iframe')).toBeNull()
    expect(screen.getByLabelText('Preview port')).toHaveValue(null)

    rerender(<PreviewTab id="sb1" status="running" defaultPort={6080} />)
    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('src')).toContain('https://6080-sb1'))
    expect(screen.getByLabelText('Preview port')).toHaveValue(6080)
  })

  it('keeps a port the user chose when the default resolves later', async () => {
    const { container, rerender } = render(<PreviewTab id="sb1" status="running" defaultPort={6080} />)
    const input = screen.getByLabelText('Preview port')
    await userEvent.clear(input)
    await userEvent.type(input, '3000{Enter}')
    rerender(<PreviewTab id="sb1" status="running" defaultPort={6081} />)
    await waitFor(() => expect(container.querySelector('iframe')?.getAttribute('src')).toContain('https://3000-sb1'))
  })
})

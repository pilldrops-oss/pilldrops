import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { PreviewTab } from '../../src/pages/SandboxDetail'
import { getSandboxPorts } from '../../src/api'
import type { SandboxPorts } from '../../src/types'

// Only getSandboxPorts is faked, so PreviewTab's iframe behaviour is exercised for real.
vi.mock('../../src/api', async importOriginal => {
  const actual = await importOriginal<typeof import('../../src/api')>()
  return { ...actual, getSandboxPorts: vi.fn() }
})

const getPortsMock = vi.mocked(getSandboxPorts)
const original = window.__SANDBOXD__

function setDomain(sandboxDomain: string | undefined) {
  window.__SANDBOXD__ = { key: '', sandboxDomain } as typeof window.__SANDBOXD__
}

function portsResp(list: SandboxPorts['ports']): SandboxPorts {
  return { ports: list }
}

describe('PreviewTab port detection', () => {
  beforeEach(() => {
    getPortsMock.mockReset()
    setDomain('pilldrops.com')
  })
  afterEach(() => {
    window.__SANDBOXD__ = original
  })

  it('renders detected listening ports as selectable chips, not the probed-but-silent ones', async () => {
    getPortsMock.mockResolvedValue(
      portsResp([
        { port: 3000, listening: true },
        { port: 8000, listening: false },
      ]),
    )
    render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    await waitFor(() => expect(screen.getByRole('button', { name: /3000/ })).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /^8000$/ })).toBeNull()
  })

  it('clicking a detected port switches the preview', async () => {
    getPortsMock.mockResolvedValue(portsResp([{ port: 3000, listening: true }]))
    const { container } = render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    const chip = await screen.findByRole('button', { name: /3000/ })
    await userEvent.click(chip)

    const iframe = container.querySelector('iframe')
    expect(iframe?.getAttribute('src')).toContain('https://3000-sb1')
  })

  it('disables a port that listens on loopback only and says why', async () => {
    getPortsMock.mockResolvedValue(portsResp([{ port: 9222, listening: true, loopback_only: true }]))
    const { container } = render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    const chip = await screen.findByRole('button', { name: /9222/ })
    expect(chip).toBeDisabled()
    expect(chip).toHaveAttribute('title', expect.stringMatching(/127\.0\.0\.1/))
    await userEvent.click(chip)
    expect(container.querySelector('iframe')?.getAttribute('src')).toContain('https://8000-sb1')
  })

  it('keeps the manual port input as a fallback alongside detected chips', async () => {
    getPortsMock.mockResolvedValue(portsResp([{ port: 3000, listening: true }]))
    render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    await screen.findByRole('button', { name: /3000/ })
    expect(screen.getByLabelText(/preview port/i)).toBeInTheDocument()
  })

  // Detection probes a fixed candidate list, not the socket table.
  it('labels detected ports as a hint rather than a guarantee', async () => {
    getPortsMock.mockResolvedValue(portsResp([{ port: 3000, listening: true }]))
    render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    await screen.findByRole('button', { name: /3000/ })
    expect(screen.getByText(/unusual port|not exhaustive|common ports/i)).toBeInTheDocument()
  })

  it('still works when detection finds nothing — no chips, manual/default port still drives the preview', async () => {
    getPortsMock.mockResolvedValue(portsResp([]))
    const { container } = render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)

    await waitFor(() => expect(getPortsMock).toHaveBeenCalledWith('sb1'))
    expect(screen.queryByRole('button', { name: /^\d+$/ })).toBeNull()
    const iframe = container.querySelector('iframe')
    expect(iframe?.getAttribute('src')).toContain('https://8000-sb1')
  })

  it('does not probe a paused sandbox', () => {
    render(<PreviewTab id="sb1" status="paused" defaultPort={8000} />)
    expect(getPortsMock).not.toHaveBeenCalled()
  })
})

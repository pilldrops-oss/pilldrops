import { describe, it, expect, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ShellTab, PreviewTab } from '../../src/pages/SandboxDetail'

const original = window.__SANDBOXD__

function setDomain(sandboxDomain: string | undefined) {
  window.__SANDBOXD__ = { key: '', sandboxDomain } as typeof window.__SANDBOXD__
}

afterEach(() => {
  window.__SANDBOXD__ = original
})

describe('PreviewTab embedding', () => {
  it('embeds when previews are a separate origin', () => {
    setDomain('pilldrops.com')
    const { container } = render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)
    expect(container.querySelector('iframe')).not.toBeNull()
  })

  // The security property: no iframe at all, not merely a notice beside one.
  it('does NOT embed when previews share the dashboard origin', () => {
    setDomain(undefined)
    const { container } = render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)
    expect(container.querySelector('iframe')).toBeNull()
    expect(screen.getByText(/opening in a new tab instead/i)).toBeInTheDocument()
  })

  it('still shows the paused state regardless of origin mode', () => {
    setDomain(undefined)
    const { container } = render(<PreviewTab id="sb1" status="paused" defaultPort={8000} />)
    expect(container.querySelector('iframe')).toBeNull()
    expect(screen.getByText(/sandbox is paused/i)).toBeInTheDocument()
  })
})

describe('ShellTab is not governed by the preview origin policy', () => {
  // The terminal is the platform's own PTY over a WebSocket, not a preview, so the preview
  // origin policy must not affect what it renders.
  it('never embeds an iframe, whatever the origin mode', () => {
    for (const domain of ['pilldrops.com', undefined]) {
      setDomain(domain)
      const { container, unmount } = render(<ShellTab id="sb1" status="running" template="base" />)
      expect(container.querySelector('iframe')).toBeNull()
      unmount()
    }
  })

  it('never shows the preview embed refusal', () => {
    setDomain(undefined)
    render(<ShellTab id="sb1" status="running" template="base" />)
    expect(screen.queryByText(/opening in a new tab instead/i)).toBeNull()
  })

  it('renders identically with and without a preview domain', () => {
    setDomain('pilldrops.com')
    const withDomain = render(<ShellTab id="sb1" status="running" template="base" />)
    const a = withDomain.container.innerHTML
    withDomain.unmount()

    setDomain(undefined)
    const without = render(<ShellTab id="sb1" status="running" template="base" />)
    const b = without.container.innerHTML
    without.unmount()

    expect(b).toBe(a)
  })

  it('still shows the paused state regardless of origin mode', () => {
    setDomain(undefined)
    const { container } = render(<ShellTab id="sb1" status="paused" template="base" />)
    expect(container.querySelector('iframe')).toBeNull()
    expect(screen.getByText(/sandbox is paused/i)).toBeInTheDocument()
  })
})

import { describe, it, expect, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { ShellTab } from '../../src/pages/SandboxDetail'

const original = window.__SANDBOXD__
afterEach(() => {
  window.__SANDBOXD__ = original
})

function setDomain(sandboxDomain?: string) {
  window.__SANDBOXD__ = { key: '', sandboxDomain } as typeof window.__SANDBOXD__
}

describe('Terminal tab', () => {
  it('offers the template quick-starts alongside the shell', () => {
    setDomain('pilldrops.com')
    render(<ShellTab id="sb1" status="running" template="go" />)
    expect(screen.getByRole('button', { name: /go version/i })).toBeInTheDocument()
  })

  it('offers the pandas demo only where the template ships pandas', () => {
    setDomain('pilldrops.com')
    const { unmount } = render(<ShellTab id="sb1" status="running" template="fastapi" />)
    expect(screen.getByRole('button', { name: /python version/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /pandas demo/i })).toBeNull()
    unmount()
    render(<ShellTab id="sb1" status="running" template="python-3.11" />)
    expect(screen.getByRole('button', { name: /pandas demo/i })).toBeInTheDocument()
  })

  it('offers the generic ones for a template with no specialisation', () => {
    setDomain('pilldrops.com')
    render(<ShellTab id="sb1" status="running" template="base" />)
    expect(screen.getByRole('button', { name: /system info/i })).toBeInTheDocument()
  })

  // The shell is a WebSocket rendered by this page; an iframe here would be the regression.
  it('renders the shell without an iframe', () => {
    setDomain('pilldrops.com')
    const { container } = render(<ShellTab id="sb1" status="running" template="base" />)
    expect(container.querySelector('iframe')).toBeNull()
  })

  it('shows the paused state instead of commands when paused', () => {
    setDomain('pilldrops.com')
    render(<ShellTab id="sb1" status="paused" template="go" />)
    expect(screen.getByText(/sandbox is paused/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /go version/i })).toBeNull()
  })

  it('works with no preview domain configured', () => {
    setDomain(undefined)
    const { container } = render(<ShellTab id="sb1" status="running" template="go" />)
    expect(container.querySelector('iframe')).toBeNull()
    expect(screen.queryByText(/opening in a new tab instead/i)).toBeNull()
    expect(screen.getByRole('button', { name: /go version/i })).toBeInTheDocument()
  })
})

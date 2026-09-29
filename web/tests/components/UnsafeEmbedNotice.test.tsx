import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import UnsafeEmbedNotice from '../../src/components/UnsafeEmbedNotice'

describe('UnsafeEmbedNotice', () => {
  it('explains why it is not embedding and links to the content', () => {
    render(<UnsafeEmbedNotice url="/preview/sb1/8000/" what="preview" />)
    expect(screen.getByText(/opening in a new tab instead/i)).toBeInTheDocument()
    const link = screen.getByRole('link', { name: /open the preview in a new tab/i })
    expect(link).toHaveAttribute('href', '/preview/sb1/8000/')
    expect(link).toHaveAttribute('target', '_blank')
  })

  it('names the terminal when that is what was blocked', () => {
    render(<UnsafeEmbedNotice url="/preview/sb1/7681/" what="terminal" />)
    expect(
      screen.getByRole('link', { name: /open the terminal in a new tab/i }),
    ).toBeInTheDocument()
  })

  // Only the operator can set server env vars.
  it('tells an admin how to re-enable embedding — and only an admin', () => {
    window.__SANDBOXD__ = { key: '', mode: 'admin' }
    const { unmount } = render(<UnsafeEmbedNotice url="/x" what="preview" />)
    expect(screen.getByText(/SANDBOXD_SANDBOX_DOMAIN/)).toBeInTheDocument()
    unmount()

    window.__SANDBOXD__ = { key: '', mode: 'user' }
    render(<UnsafeEmbedNotice url="/x" what="preview" />)
    expect(screen.queryByText(/SANDBOXD_SANDBOX_DOMAIN/)).toBeNull()
  })

  it('opens without leaking a window.opener handle', () => {
    render(<UnsafeEmbedNotice url="/x" what="preview" />)
    expect(screen.getByRole('link')).toHaveAttribute('rel', 'noopener noreferrer')
  })
})

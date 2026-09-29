import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { SandboxRowItem } from '../../src/pages/Sandboxes'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import type { SandboxRow, Template } from '../../src/types'

vi.mock('../../src/api', () => ({
  previewUrl: vi.fn(), getSandbox: vi.fn(), pauseSandbox: vi.fn(), resumeSandbox: vi.fn(),
  destroySandbox: vi.fn(), renameSandbox: vi.fn(), getUsage: vi.fn(), getConcurrency: vi.fn(),
  getEvents: vi.fn(), listTemplates: vi.fn(), listSandboxes: vi.fn(),
}))

const tpl = (over: Partial<Template>): Template => ({
  id: 'x', name: 'x', description: '', tags: [], language: '', cpu: 1, memory_mb: 512,
  visibility: 'public', preinstalled: [], has_server: false, default_port: 0, ...over,
})

const row = (template: string, over: Partial<SandboxRow> = {}): SandboxRow => ({
  id: `sb-${template}`, template, status: 'running', boot_type: 'cold', boot_ms: 1,
  created_at: new Date().toISOString(), expires_at: '', ...over,
})

function renderRow(r: SandboxRow, t?: Template) {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <ConfirmProvider>
          <SandboxRowItem row={r} template={t} onDone={() => {}} />
        </ConfirmProvider>
      </ToastProvider>
    </MemoryRouter>,
  )
}

describe('sandbox row preview action', () => {
  it('is "Desktop" for a GUI template and "Open preview" otherwise', () => {
    const { unmount } = renderRow(row('desktop'), tpl({ id: 'desktop', has_server: true, tags: ['vnc'] }))
    const desktop = screen.getByRole('button', { name: 'Desktop' }).title
    unmount()

    renderRow(row('python-3.11'), tpl({ id: 'python-3.11', has_server: true }))
    const plain = screen.getByRole('button', { name: 'Open preview' }).title
    expect(desktop).toBe('Desktop')
    expect(plain).toBe('Open preview')
    expect(desktop).not.toBe(plain)
  })

  it('falls back to "Open preview" when the catalog entry is unknown', () => {
    renderRow(row('custom'))
    expect(screen.getByRole('button', { name: 'Open preview' })).toBeInTheDocument()
  })

  it('greys the Desktop action when the sandbox reports has_desktop false', () => {
    renderRow(row('desktop', { has_desktop: false }), tpl({ id: 'desktop', has_server: true, tags: ['vnc'] }))
    const btn = screen.getByRole('button', { name: 'This template has no desktop' })
    expect(btn).toBeDisabled()
  })

  it('leaves the Desktop action enabled when has_desktop is true', () => {
    renderRow(row('desktop', { has_desktop: true }), tpl({ id: 'desktop', has_server: true, tags: ['vnc'] }))
    expect(screen.getByRole('button', { name: 'Desktop' })).toBeEnabled()
  })

  // A node too old to report the field omits it; absent is not false, and
  // greying on absent would disable a button that works.
  it('leaves the Desktop action enabled when has_desktop is absent', () => {
    renderRow(row('desktop'), tpl({ id: 'desktop', has_server: true, tags: ['vnc'] }))
    expect(screen.getByRole('button', { name: 'Desktop' })).toBeEnabled()
  })

  // Greying must key on the desktop action, not on the field alone: a plain
  // template legitimately has no desktop and its preview must still open.
  it('never greys a plain template\'s preview', () => {
    renderRow(row('python-3.11', { has_desktop: false }), tpl({ id: 'python-3.11', has_server: true }))
    expect(screen.getByRole('button', { name: 'Open preview' })).toBeEnabled()
  })
})

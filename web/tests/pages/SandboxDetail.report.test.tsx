import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LogsTab, PreviewTab } from '../../src/pages/SandboxDetail'
import { getSandboxLogs, getSandboxPorts } from '../../src/api'
import { composeCreateBody } from '../../src/lib/launchBody'
import type { Template } from '../../src/types'

vi.mock('../../src/api', async importOriginal => {
  const actual = await importOriginal<typeof import('../../src/api')>()
  return { ...actual, getSandboxPorts: vi.fn(), getSandboxLogs: vi.fn() }
})
vi.mock('../../src/lib/previewToken', () => ({
  usePreviewLink: (id: string, port: string) => `/preview/${id}/${port}/?token=tok`,
}))

const original = window.__SANDBOXD__

describe('playwright report and logs (UC4)', () => {
  beforeEach(() => {
    window.__SANDBOXD__ = { key: '' } as typeof window.__SANDBOXD__
    vi.mocked(getSandboxPorts).mockResolvedValue({ ports: [] })
  })
  afterEach(() => {
    window.__SANDBOXD__ = original
  })

  it('links straight to report/ on the same preview URL, token kept', async () => {
    render(<PreviewTab id="sb1" status="running" defaultPort={8000} outcome="report" />)
    const link = await screen.findByRole('link', { name: /View report/ })
    expect(link).toHaveAttribute('href', '/preview/sb1/8000/report/?token=tok')
  })

  it('shows no report link for an ordinary preview', async () => {
    render(<PreviewTab id="sb1" status="running" defaultPort={8000} />)
    await screen.findByRole('link', { name: /Open in new tab/ })
    expect(screen.queryByRole('link', { name: /View report/ })).toBeNull()
  })

  it('does not chain a second report server onto a template that serves its own report', () => {
    const tpl = { id: 'playwright', outcome: 'report', has_server: false } as Template
    const body = composeCreateBody({
      tpl, kind: 'test', testCmd: 'npx playwright test', serveReport: true,
      start: '', port: '8000', envList: [], previewPort: 8000,
    })
    expect(body.start).toBe('npx playwright test')
  })

  it('strips ANSI escapes from the log feed and has an auto-scroll toggle', async () => {
    Element.prototype.scrollIntoView = vi.fn()
    vi.mocked(getSandboxLogs).mockResolvedValue({ lines: ['\x1b[31mRED\x1b[0m done', '\x1b]0;title\x07plain'] })
    render(<LogsTab id="sb1" />)
    expect(await screen.findByText('RED done')).toBeInTheDocument()
    expect(screen.getByText('plain')).toBeInTheDocument()
    const toggle = screen.getByRole('checkbox', { name: /auto-scroll/i })
    expect(toggle).toBeChecked()
    await userEvent.click(toggle)
    await waitFor(() => expect(toggle).not.toBeChecked())
  })

  it('shows its own copy for the server\'s no-app-log note, not the raw text', async () => {
    Element.prototype.scrollIntoView = vi.fn()
    vi.mocked(getSandboxLogs).mockResolvedValue({
      lines: [], note: 'no app log yet — it captures the server started via `--start`/the start API',
    })
    const { container } = render(<LogsTab id="sb1" />)
    await waitFor(() => expect(getSandboxLogs).toHaveBeenCalled())
    expect(await screen.findByRole('heading', { name: 'No app logs yet' })).toBeInTheDocument()
    expect(container.textContent).not.toContain('`')
  })

  it('keeps any other server note as the heading', async () => {
    Element.prototype.scrollIntoView = vi.fn()
    vi.mocked(getSandboxLogs).mockResolvedValue({ lines: [], note: 'sandbox is paused — logs return when it wakes' })
    render(<LogsTab id="sb1" />)
    expect(await screen.findByRole('heading', { name: /sandbox is paused/ })).toBeInTheDocument()
  })
})

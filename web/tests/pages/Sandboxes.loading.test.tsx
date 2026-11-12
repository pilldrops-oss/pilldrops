import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { HashRouter } from 'react-router-dom'
import Sandboxes from '../../src/pages/Sandboxes'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  listSandboxes: vi.fn(),
  listTemplates: vi.fn(),
  getUsage: vi.fn(),
  getConcurrency: vi.fn(),
  getEvents: vi.fn(),
}))
vi.mock('../../src/hooks/useSSE', () => ({ useSSE: () => ({ data: null, error: null }) }))

function renderPage() {
  render(
    <HashRouter>
      <ToastProvider>
        <ConfirmProvider>
          <Sandboxes />
        </ConfirmProvider>
      </ToastProvider>
    </HashRouter>,
  )
}

beforeEach(() => {
  vi.mocked(api.listTemplates).mockResolvedValue([])
  vi.mocked(api.getUsage).mockResolvedValue({} as never)
  vi.mocked(api.getConcurrency).mockResolvedValue({ points: [] } as never)
  vi.mocked(api.getEvents).mockResolvedValue({ events: [] } as never)
})

describe('sandboxes list before the first response', () => {
  it('claims neither zero sandboxes nor an empty account while loading', () => {
    vi.mocked(api.listSandboxes).mockReturnValue(new Promise(() => {}))
    renderPage()
    expect(screen.queryByText(/0 sandboxes/)).toBeNull()
    expect(screen.queryByText('Launch your first sandbox')).toBeNull()
  })

  it('shows the counts and the empty state once the list arrives empty', async () => {
    vi.mocked(api.listSandboxes).mockResolvedValue({ sandboxes: [] })
    renderPage()
    expect(await screen.findByText('Launch your first sandbox')).toBeInTheDocument()
    expect(screen.getByText(/0 sandboxes/)).toBeInTheDocument()
  })
})

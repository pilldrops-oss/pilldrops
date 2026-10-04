import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route, useNavigate } from 'react-router-dom'
import Deploy from '../../src/pages/Deploy'
import * as api from '../../src/api'
import type { Template, CreateSandboxResp } from '../../src/types'

vi.mock('../../src/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../../src/api')>()),
  listTemplates: vi.fn(),
  listPublicTemplates: vi.fn(),
  listNodes: vi.fn(),
  createSandbox: vi.fn(),
  destroySandbox: vi.fn(),
  getPreviewStatus: vi.fn(),
  getSandbox: vi.fn(),
  postLaunchSample: vi.fn(),
}))

const base: Template = {
  id: 'base', name: 'Base', description: 'Minimal Debian', cpu: 1, memory_mb: 512, visibility: 'public',
}
const resp: CreateSandboxResp = {
  id: 'sb1', template: 'base', status: 'running', created_at: '', expires_at: '',
}

beforeEach(() => {
  vi.mocked(api.listTemplates).mockResolvedValue([base])
  vi.mocked(api.listPublicTemplates).mockResolvedValue([])
  vi.mocked(api.listNodes).mockResolvedValue({ nodes: [] } as never)
  vi.mocked(api.createSandbox).mockResolvedValue(resp)
  vi.mocked(api.destroySandbox).mockResolvedValue(undefined as never)
  vi.mocked(api.getSandbox).mockResolvedValue({ ...resp } as never)
  vi.mocked(api.postLaunchSample).mockResolvedValue(undefined as never)
})

// A button standing in for the top bar's "+ New" / nav link, which calls
// navigate('/deploy') while Deploy is already mounted at that same route.
function Harness() {
  const navigate = useNavigate()
  return (
    <>
      <button onClick={() => navigate('/deploy')}>go to deploy</button>
      <Routes>
        <Route path="/deploy" element={<Deploy />} />
      </Routes>
    </>
  )
}

describe('Deploy resets its launch state on navigation (B21)', () => {
  it('drops the previous "ready" screen when the route is revisited', async () => {
    render(
      <MemoryRouter initialEntries={['/deploy']}>
        <Harness />
      </MemoryRouter>,
    )

    await userEvent.click(await screen.findByRole('button', { name: '⚡ Quick launch' }))
    await screen.findByText('Sandbox is ready')

    await userEvent.click(screen.getByRole('button', { name: 'go to deploy' }))

    await waitFor(() => expect(screen.queryByText('Sandbox is ready')).toBeNull())
  })
})

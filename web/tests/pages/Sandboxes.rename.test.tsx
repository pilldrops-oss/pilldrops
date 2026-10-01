import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HashRouter } from 'react-router-dom'
import { ToastProvider } from '../../src/components/Toast'
import { InlineRename } from '../../src/pages/Sandboxes'
import { renameSandbox } from '../../src/api'
import type { SandboxDetail } from '../../src/types'

vi.mock('../../src/api', () => ({
  renameSandbox: vi.fn(),
}))

const renameMock = vi.mocked(renameSandbox)

function renderInline(overrides: Partial<{ id: string; name: string }> = {}) {
  const onRenamed = vi.fn()
  render(
    // The label is a real link to the detail page, so it needs a router.
    <HashRouter>
      <ToastProvider>
        <InlineRename id={overrides.id ?? 'sb_a91f2c'} name={overrides.name ?? ''} onRenamed={onRenamed} />
      </ToastProvider>
    </HashRouter>,
  )
  return { onRenamed }
}

function detail(name: string): SandboxDetail {
  return {
    id: 'sb_a91f2c', template: 'python-3.11', name,
    status: 'running', boot_type: '', boot_ms: 0, created_at: '', expires_at: '',
  }
}

describe('InlineRename', () => {
  beforeEach(() => {
    renameMock.mockReset()
  })

  it('renders the id as the primary label when unnamed', () => {
    renderInline({ id: 'sb_a91f2c', name: '' })
    expect(screen.getByText('sb_a91f2c')).toBeInTheDocument()
  })

  it('renders the name as the primary label, with the id kept visible as a secondary line', () => {
    renderInline({ id: 'sb_a91f2c', name: 'staging-worker' })
    expect(screen.getByText('staging-worker')).toBeInTheDocument()
    expect(screen.getByText('sb_a91f2c')).toBeInTheDocument()
  })

  it('turns the label into an input when the pencil is clicked', async () => {
    renderInline({ id: 'sb1', name: 'my-box' })
    await userEvent.click(screen.getByRole('button', { name: /rename sandbox/i }))
    const input = screen.getByRole('textbox', { name: /sandbox name/i })
    expect(input).toHaveValue('my-box')
  })

  it('commits the new name on Enter and reports the server-sanitized value', async () => {
    renameMock.mockResolvedValue(detail('renamed-box'))
    const { onRenamed } = renderInline({ id: 'sb1', name: 'my-box' })

    await userEvent.click(screen.getByRole('button', { name: /rename sandbox/i }))
    const input = screen.getByRole('textbox', { name: /sandbox name/i })
    await userEvent.clear(input)
    await userEvent.type(input, 'renamed-box{Enter}')

    await waitFor(() => expect(renameMock).toHaveBeenCalledWith('sb1', 'renamed-box'))
    await waitFor(() => expect(onRenamed).toHaveBeenCalledWith('renamed-box'))
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(await screen.findByText('Renamed to renamed-box')).toBeInTheDocument()
  })

  it('cancels on Escape without calling the API, reverting to the original value', async () => {
    const { onRenamed } = renderInline({ id: 'sb1', name: 'my-box' })

    await userEvent.click(screen.getByRole('button', { name: /rename sandbox/i }))
    const input = screen.getByRole('textbox', { name: /sandbox name/i })
    await userEvent.type(input, 'discard-me')
    fireEvent.keyDown(input, { key: 'Escape' })

    expect(renameMock).not.toHaveBeenCalled()
    expect(onRenamed).not.toHaveBeenCalled()
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.getByText('my-box')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: /rename sandbox/i }))
    expect(screen.getByRole('textbox', { name: /sandbox name/i })).toHaveValue('my-box')
  })

  it('does not call the API when Enter is pressed with no change', async () => {
    renderInline({ id: 'sb1', name: 'my-box' })
    await userEvent.click(screen.getByRole('button', { name: /rename sandbox/i }))
    fireEvent.keyDown(screen.getByRole('textbox', { name: /sandbox name/i }), { key: 'Enter' })
    expect(renameMock).not.toHaveBeenCalled()
  })
})

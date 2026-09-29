import { describe, it, expect, vi, afterEach } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { useGitRepo } from '../../src/hooks/useGitRepo'

// The probe reads raw.githubusercontent.com; stub it so the test never
// depends on the network.
afterEach(() => { vi.unstubAllGlobals() })

describe('useGitRepo', () => {
  it('parses a repo only while the git source is selected', () => {
    const { result, rerender } = renderHook(({ on }) => useGitRepo(on), {
      initialProps: { on: true },
    })
    act(() => { result.current.setUrl('https://github.com/owner/repo') })
    expect(result.current.repo).not.toBeNull()

    rerender({ on: false })
    expect(result.current.repo).toBeNull()
    // Switching tabs must not discard what the user typed.
    expect(result.current.url).toBe('https://github.com/owner/repo')
  })

  it('drops a previous probe result when the url changes', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 404 })))
    const { result } = renderHook(() => useGitRepo(true))
    act(() => { result.current.setUrl('owner/repo') })
    await act(async () => { await result.current.runProbe() })
    await waitFor(() => expect(result.current.probe).not.toBe('checking'))

    act(() => { result.current.setUrl('owner/other') })
    expect(result.current.probe).toBe('idle')
    expect(result.current.files).toEqual([])
  })
})

import { describe, it, expect, beforeEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useIsDarkTheme } from '../../src/components/useIsDarkTheme'

// Reset before, not after: an afterEach reset fires the previous test's still-mounted
// observer outside act() and React warns.
beforeEach(() => document.body.classList.remove('dark'))

describe('useIsDarkTheme', () => {
  it('reports the theme already on the body at mount', () => {
    document.body.classList.add('dark')
    const { result } = renderHook(() => useIsDarkTheme())
    expect(result.current).toBe(true)
  })

  it('defaults to light', () => {
    const { result } = renderHook(() => useIsDarkTheme())
    expect(result.current).toBe(false)
  })

  it('follows the theme toggle both ways', async () => {
    const { result } = renderHook(() => useIsDarkTheme())
    expect(result.current).toBe(false)

    // MutationObserver fires as a microtask, so a sync act() exits before the update lands.
    await act(async () => { document.body.classList.add('dark') })
    expect(result.current).toBe(true)

    await act(async () => { document.body.classList.remove('dark') })
    expect(result.current).toBe(false)
  })

  it('stops observing after unmount', async () => {
    const { result, unmount } = renderHook(() => useIsDarkTheme())
    unmount()
    act(() => { document.body.classList.add('dark') })
    expect(result.current).toBe(false)
  })
})

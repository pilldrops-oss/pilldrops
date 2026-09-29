import { describe, it, expect, vi } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { useApi, usePoll } from '../../src/hooks/useApi'

// A slow first request must not overwrite a newer one's result. Without the
// generation guard this is the classic "stale response wins" bug: the user
// switches filters, the old request lands last, and the screen shows old data.
function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

describe('useApi', () => {
  it('discards a response from a superseded run', async () => {
    const slow = deferred<string>()
    const fast = deferred<string>()
    const calls = [slow, fast]
    let n = 0
    const { result } = renderHook(() => useApi(() => calls[n++].promise))

    await waitFor(() => expect(n).toBe(1))
    act(() => { result.current.refetch() })
    await waitFor(() => expect(n).toBe(2))

    await act(async () => { fast.resolve('new'); await fast.promise })
    await act(async () => { slow.resolve('old'); await slow.promise })

    expect(result.current.data).toBe('new')
  })

  it('reports the raw error so callers can inspect status codes', async () => {
    const boom = new Error('nope')
    const { result } = renderHook(() => useApi(() => Promise.reject(boom)))
    await waitFor(() => expect(result.current.error).toBe(boom))
    expect(result.current.data).toBeNull()
  })
})

describe('usePoll', () => {
  it('keeps the last good data when a later poll fails', async () => {
    vi.useFakeTimers()
    let call = 0
    const fetcher = () =>
      ++call === 1 ? Promise.resolve(['a']) : Promise.reject(new Error('flaky'))

    const { result } = renderHook(() => usePoll(fetcher, 1000))
    await act(async () => { await Promise.resolve() })
    expect(result.current.data).toEqual(['a'])

    await act(async () => { vi.advanceTimersByTime(1000); await Promise.resolve() })
    expect(result.current.data).toEqual(['a'])
    expect(result.current.error).toBeInstanceOf(Error)
    vi.useRealTimers()
  })
})

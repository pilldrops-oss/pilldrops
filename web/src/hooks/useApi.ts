import { useState, useEffect, useCallback, useRef } from 'react'

interface ApiState<T> {
  data: T | null
  loading: boolean
  error: unknown
}

// Every run takes a generation number. Changing deps, unmounting, or starting a
// newer run bumps the counter, so a slow response that lands after the fact is
// dropped instead of overwriting fresher state.

export function useApi<T>(fetcher: () => Promise<T>, deps: unknown[] = []) {
  const [state, setState] = useState<ApiState<T>>({ data: null, loading: true, error: null })
  const gen = useRef(0)

  const run = useCallback(async () => {
    const mine = ++gen.current
    setState(s => ({ ...s, loading: true, error: null }))
    try {
      const data = await fetcher()
      if (gen.current === mine) setState({ data, loading: false, error: null })
    } catch (e) {
      if (gen.current === mine) setState({ data: null, loading: false, error: e })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  useEffect(() => {
    run()
    // eslint-disable-next-line react-hooks/exhaustive-deps -- gen is a counter, not a node ref; the cleanup must bump the live value
    return () => { gen.current++ }
  }, [run])

  return { ...state, refetch: run }
}

export function usePoll<T>(fetcher: () => Promise<T>, intervalMs: number, deps: unknown[] = []) {
  const [state, setState] = useState<ApiState<T>>({ data: null, loading: true, error: null })
  const gen = useRef(0)

  const run = useCallback(async () => {
    const mine = ++gen.current
    try {
      const data = await fetcher()
      if (gen.current === mine) setState({ data, loading: false, error: null })
    } catch (e) {
      // A transient poll failure must not wipe an already-loaded screen.
      if (gen.current === mine) setState(s => ({ ...s, loading: false, error: e }))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  useEffect(() => {
    run()
    const id = setInterval(run, intervalMs)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- gen is a counter, not a node ref; the cleanup must bump the live value
    return () => { gen.current++; clearInterval(id) }
  }, [run, intervalMs])

  return { ...state, refetch: run }
}

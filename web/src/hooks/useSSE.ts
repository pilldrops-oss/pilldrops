import { useEffect, useRef, useState, useCallback } from 'react'

export function useSSE<T>(path: string, eventName: string) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    const es = new EventSource(path)
    esRef.current = es

    es.addEventListener(eventName, (e: MessageEvent) => {
      try {
        setData(JSON.parse(e.data) as T)
        setError(null)
      } catch {
        setError('Parse error')
      }
    })

    es.onerror = () => {
      setError('SSE connection error')
    }

    return () => {
      es.close()
      esRef.current = null
    }
  }, [path, eventName])

  return { data, error }
}

// Collect a rolling buffer of SSE events
export function useSSEBuffer<T>(path: string, eventName: string, maxItems = 200) {
  const [items, setItems] = useState<T[]>([])
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    const es = new EventSource(path)
    esRef.current = es

    es.addEventListener(eventName, (e: MessageEvent) => {
      try {
        const item = JSON.parse(e.data) as T
        setItems(prev => [item, ...prev].slice(0, maxItems))
      } catch {
        // ignore parse errors
      }
    })

    return () => {
      es.close()
      esRef.current = null
    }
  }, [path, eventName, maxItems])

  return items
}

// useSSEBuffer but appending (oldest first, like logs)
export function useSSELog<T>(path: string, eventName: string, maxItems = 500) {
  const [items, setItems] = useState<T[]>([])
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    const es = new EventSource(path)
    esRef.current = es

    es.addEventListener(eventName, (e: MessageEvent) => {
      try {
        const item = JSON.parse(e.data) as T
        setItems(prev => [...prev, item].slice(-maxItems))
      } catch {
        // ignore
      }
    })

    return () => {
      es.close()
      esRef.current = null
    }
  }, [path, eventName, maxItems])

  const clear = useCallback(() => setItems([]), [])
  return { items, clear }
}

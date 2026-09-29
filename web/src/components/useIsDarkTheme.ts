import { useEffect, useState } from 'react'

export function useIsDarkTheme(): boolean {
  const [dark, setDark] = useState(() => document.body.classList.contains('dark'))

  useEffect(() => {
    const sync = () =>
      setDark(prev => {
        const now = document.body.classList.contains('dark')
        return prev === now ? prev : now
      })
    sync()
    const observer = new MutationObserver(sync)
    observer.observe(document.body, { attributes: true, attributeFilter: ['class'] })
    return () => observer.disconnect()
  }, [])

  return dark
}

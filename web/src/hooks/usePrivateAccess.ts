import { useEffect, useState } from 'react'
import { getCapabilities } from '../api'

// Whether this server offers private access; null until known, false when the answer cannot be read.
export function usePrivateAccess(): boolean | null {
  const [on, setOn] = useState<boolean | null>(null)
  useEffect(() => {
    let live = true
    getCapabilities()
      .then(c => { if (live) setOn(!!c.private_access) })
      .catch(() => { if (live) setOn(false) })
    return () => { live = false }
  }, [])
  return on
}

import { useEffect, useState } from 'react'
import { getPreviewToken, previewUrl } from '../api'

// Preview tokens are scoped to (sandbox, port). The cache holds promises so
// concurrent renders asking for the same port share one request.
const cache = new Map<string, Promise<string | null>>()

const key = (id: string, port: number | string) => `${id}:${port}`

/**
 * previewToken returns the token for one port of one sandbox, or null when none
 * is available (public sandbox, no permission); the preview gate decides then.
 */
export function previewToken(id: string, port: number | string): Promise<string | null> {
  const k = key(id, port)
  const hit = cache.get(k)
  if (hit) return hit

  const p = getPreviewToken(id, port).catch(() => null)
  cache.set(k, p)
  // Do not cache failures; a token missing during boot appears once the sandbox is placed.
  void p.then(tok => {
    if (tok === null) cache.delete(k)
  })
  return p
}

/** previewLink returns the preview URL for a port with the token attached when available. */
export async function previewLink(id: string, port: number | string): Promise<string> {
  const base = previewUrl(id, port)
  const tok = await previewToken(id, port)
  if (!tok) return base
  return `${base}${base.includes('?') ? '&' : '?'}token=${encodeURIComponent(tok)}`
}

/** forgetPreviewTokens drops a sandbox's cached tokens on destroy or recreate. */
export function forgetPreviewTokens(id: string): void {
  for (const k of [...cache.keys()]) {
    if (k.startsWith(`${id}:`)) cache.delete(k)
  }
}

/** Test seam. */
export function _resetPreviewTokenCache(): void {
  cache.clear()
}

/**
 * usePreviewLink is the hook form of previewLink. Returns '' until the token
 * resolves, so the iframe never flashes the un-tokened "Private sandbox" page.
 */
export function usePreviewLink(
  id: string | undefined,
  port: number | string | undefined | null,
): string {
  const [url, setUrl] = useState('')
  useEffect(() => {
    if (!id || !port) {
      setUrl('')
      return
    }
    let live = true
    void previewLink(id, port).then(u => {
      if (live) setUrl(u)
    })
    return () => {
      live = false
    }
  }, [id, port])
  return url
}

// Validated in the browser because a bad gateway URL fails much later, as an opaque
// error that reads like a broken API key.

export type GatewayUrlResult =
  | { ok: true; url: string; note?: string }
  | { ok: false; error: string }

/** validateGatewayUrl checks and normalises a base URL; a trailing /v1 is stripped, since the client adds it. */
export function validateGatewayUrl(raw: string): GatewayUrlResult {
  const trimmed = raw.trim()
  if (!trimmed) return { ok: false, error: 'Enter a URL, or clear the field to use the provider directly.' }

  if (/\s/.test(trimmed)) return { ok: false, error: 'A URL cannot contain spaces.' }

  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return { ok: false, error: 'That is not a valid URL — include the scheme, e.g. https://gateway.example.com' }
  }

  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
    return { ok: false, error: `Use an http:// or https:// URL (got ${parsed.protocol.replace(':', '')}).` }
  }
  if (!parsed.hostname) return { ok: false, error: 'That URL has no host.' }

  const cleaned = trimmed.replace(/\/+$/, '').replace(/\/v1$/, '').replace(/\/+$/, '')
  const note = cleaned !== trimmed.replace(/\/+$/, '')
    ? 'Saved without the trailing /v1 — the client adds the version itself.'
    : undefined

  if (parsed.protocol === 'http:') {
    return {
      ok: true,
      url: cleaned,
      note: 'This is an unencrypted http:// endpoint — your API key will be sent in clear text.',
    }
  }
  return { ok: true, url: cleaned, note }
}

// errText turns an unknown thrown value into a short, human-readable message.
export function errText(e: unknown): string {
  let msg = ''
  if (e instanceof Error) msg = e.message
  else if (typeof e === 'string') msg = e
  else if (e != null) {
    try { msg = JSON.stringify(e) } catch { msg = String(e) }
  }
  if (!msg || msg === '[object Object]' || msg === '{}') {
    return 'Something went wrong. Please try again.'
  }

  // Strip the "METHOD /path -> STATUS: " prefix from legacy request() errors.
  const arrow = msg.match(/→\s*\d{3}:\s*([\s\S]*)$/)
  let body = arrow ? arrow[1].trim() : msg

  if (body.startsWith('{')) {
    try {
      const j = JSON.parse(body)
      body = j.error || j.message || j.detail || body
    } catch { /* not JSON; keep the raw body */ }
  }
  return body || msg
}

// isCreditError detects the out-of-credits / 402 / quota family.
export function isCreditError(e: unknown): boolean {
  const s = (e instanceof Error ? e.message : String(e)).toLowerCase()
  return /\b402\b|out of credit|insufficient credit|no credit|payment required|quota exceeded|over.?limit/.test(s)
}

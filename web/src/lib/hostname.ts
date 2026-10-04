// Mirrors the server's validDomainHost (internal/orchestrator/domains.go):
// lower-cased, no trailing dot, RFC 1123 labels, at least two of them,
// optionally a leading "*." for a wildcard. Catches a bad hostname before the
// round trip instead of after a 400.
export function invalidHostnameReason(raw: string): string | null {
  let host = raw.trim().toLowerCase()
  host = host.replace(/\.$/, '')
  const bare = host.replace(/^\*\./, '')
  if (!bare || bare.length > 253) return 'Enter a hostname like app.example.com'
  const labels = bare.split('.')
  if (labels.length < 2) return 'Needs at least two labels, like app.example.com'
  for (const l of labels) {
    if (!l || l.length > 63 || l.startsWith('-') || l.endsWith('-') || !/^[a-z0-9-]+$/.test(l)) {
      return `"${l}" is not a valid DNS label`
    }
  }
  return null
}

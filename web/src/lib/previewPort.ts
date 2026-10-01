import type { SandboxDetail, Template } from '../types'

// portOfUrl reads the port from a node's preview URL, path form
// (/preview/<id>/<port>/) or host form (<port>-<id>.<domain>).
function portOfUrl(raw?: string): number | undefined {
  const m = raw?.match(/\/preview\/[^/]+\/(\d+)|^https?:\/\/(\d+)-/)
  return m ? Number(m[1] ?? m[2]) : undefined
}

// previewPortOf is the port a sandbox's preview opens on. The detail GET omits
// public_port but its url is built on it, so the url is the next best answer.
export function previewPortOf(
  sb: Pick<SandboxDetail, 'public_port' | 'url' | 'desktop_url'>,
  tpl?: Pick<Template, 'default_port' | 'port'> | null,
): number {
  return sb.public_port || portOfUrl(sb.url) || portOfUrl(sb.desktop_url) || tpl?.default_port || tpl?.port || 8000
}

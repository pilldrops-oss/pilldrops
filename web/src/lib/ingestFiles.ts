export type SourceFile = { path: string; content: string }

// A sandbox is seeded with source, not with a build output. Anything over the
// size cap, any vendored or generated directory, and anything binary is
// dropped rather than uploaded and then ignored on the far side.
export const MAX_FILE = 512 * 1024

const SKIP_DIR = /(^|\/)(node_modules|\.git|dist|build|\.next|target|\.venv|__pycache__)(\/|$)/
const BIN_EXT = /\.(png|jpe?g|gif|webp|ico|pdf|gz|tar|woff2?|ttf|otf|mp4|mov|wasm|so|dylib|class|jar|zip)$/i
const MACOSX = /(^|\/)__MACOSX(\/|$)/

export function isIngestable(path: string, size?: number): boolean {
  if (SKIP_DIR.test(path) || BIN_EXT.test(path) || MACOSX.test(path)) return false
  return size == null || size <= MAX_FILE
}

/**
 * stripCommonRoot removes the single wrapper directory a zip or a dragged
 * folder adds, so `my-app/src/index.js` is uploaded as `src/index.js`. A set
 * that does not entirely share one root is left alone.
 */
export function stripCommonRoot(files: SourceFile[]): SourceFile[] {
  if (files.length < 2) return files
  const first = files[0].path.split('/')[0]
  if (!first || !files.every(f => f.path.startsWith(`${first}/`))) return files
  return files.map(f => ({ ...f, path: f.path.slice(first.length + 1) }))
}

/** mergeByPath adds `incoming` over `existing`, with a later file winning. */
export function mergeByPath(existing: SourceFile[], incoming: SourceFile[]): SourceFile[] {
  const map = new Map(existing.map(f => [f.path, f]))
  for (const f of incoming) map.set(f.path, f)
  return Array.from(map.values())
}

type ZipEntry = { dir: boolean; name: string; async: (t: 'string') => Promise<string> }

async function ingestZip(file: File): Promise<SourceFile[]> {
  // jszip is only pulled in when someone actually drops an archive.
  const { default: JSZip } = await import('jszip')
  const zip = await JSZip.loadAsync(file)
  const out: SourceFile[] = []
  for (const e of Object.values(zip.files) as ZipEntry[]) {
    if (e.dir || !isIngestable(e.name)) continue
    try {
      const content = await e.async('string')
      if (content.length <= MAX_FILE) out.push({ path: e.name, content })
    } catch {}
  }
  return out
}

export async function ingestFiles(fileList: FileList): Promise<SourceFile[]> {
  const out: SourceFile[] = []
  for (const f of Array.from(fileList)) {
    if (f.name.toLowerCase().endsWith('.zip')) {
      out.push(...(await ingestZip(f)))
      continue
    }
    const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
    if (!isIngestable(rel, f.size)) continue
    try {
      out.push({ path: rel, content: await f.text() })
    } catch {}
  }
  return out
}

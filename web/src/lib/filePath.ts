// The filesystem API spells the root "."; joining must not produce "./src", or the same
// path gets two spellings that compare unequal as tree-state keys.

export const ROOT = '.'

/** joinPath joins a directory and child name, keeping the root implicit. */
export function joinPath(dir: string, name: string): string {
  if (!dir || dir === ROOT) return name
  return `${dir.replace(/\/+$/, '')}/${name}`
}

export function baseName(path: string): string {
  const parts = path.split('/').filter(Boolean)
  return parts.length ? parts[parts.length - 1] : path
}

export function depthOf(path: string): number {
  if (!path || path === ROOT) return 0
  return path.split('/').filter(Boolean).length
}

import type { Template } from '../types'

// Search synonyms, client-side only. Distinct from the server's create-time
// aliases: "e2e" should find Playwright but is not a valid template id.
const SYNONYMS: Record<string, string[]> = {
  playwright: ['e2e', 'end-to-end', 'cypress', 'puppeteer', 'selenium', 'browser testing', 'ui test'],
  'code-interpreter': ['jupyter', 'notebook', 'pandas', 'data science', 'python'],
  desktop: ['gui', 'vnc', 'computer use', 'fluxbox'],
  browser: ['chromium', 'chrome', 'headless', 'scraping'],
  node: ['nodejs', 'typescript', 'ts', 'javascript', 'js'],
  go: ['golang'],
  rust: ['cargo', 'rustc'],
}

// One-edit Damerau-Levenshtein: a substitution, an insertion/deletion, or an
// adjacent transposition ("postgrse").
function withinOneEdit(a: string, b: string): boolean {
  if (a === b) return true
  if (Math.abs(a.length - b.length) > 1) return false

  if (a.length === b.length) {
    const diffs: number[] = []
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) diffs.push(i)
    if (diffs.length === 1) return true
    if (diffs.length === 2) {
      const [i, j] = diffs
      if (j === i + 1 && a[i] === b[j] && a[j] === b[i]) return true
    }
    return false
  }

  const shorter = a.length < b.length ? a : b
  const longer = a.length < b.length ? b : a
  let i = 0, j = 0, skipped = false
  while (i < shorter.length && j < longer.length) {
    if (shorter[i] === longer[j]) { i++; j++; continue }
    if (skipped) return false
    skipped = true
    j++
  }
  return true
}

/** scoreTemplate ranks a template against a query; 0 means no match and the caller filters on it. */
export function scoreTemplate(t: Template, query: string): number {
  const q = query.trim().toLowerCase()
  if (!q) return 1

  const id = t.id.toLowerCase()
  const name = (t.name ?? '').toLowerCase()
  const tags = (t.tags ?? []).map(s => s.toLowerCase())
  const desc = (t.description ?? '').toLowerCase()

  if (id === q || name === q) return 100
  if (id.startsWith(q) || name.startsWith(q)) return 80
  if (SYNONYMS[t.id]?.some(s => s.includes(q) || q.includes(s))) return 60
  if (tags.some(tag => tag === q || tag.startsWith(q))) return 50
  if (id.includes(q) || name.includes(q)) return 40
  if (desc.includes(q)) return 20
  // Typo tolerance only once a single edit is not most of the word.
  if (q.length >= 4 && (withinOneEdit(q, id) || withinOneEdit(q, name))) return 15
  return 0
}

export type SortKey = 'recommended' | 'name'

export const SORT_LABELS: Record<SortKey, string> = {
  recommended: 'Recommended',
  name: 'Name',
}

/** sortTemplates sorts a filtered list; with a query present, relevance wins over the picker. */
export function sortTemplates(list: Template[], key: SortKey, query: string): Template[] {
  const out = [...list]
  if (query.trim()) {
    return out.sort((a, b) => scoreTemplate(b, query) - scoreTemplate(a, query))
  }
  if (key === 'name') {
    return out.sort((a, b) => (a.name ?? a.id).localeCompare(b.name ?? b.id))
  }
  return out // 'recommended' is curated catalog order
}

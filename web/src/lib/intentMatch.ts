import type { Template } from '../types'

// Matches a described need against each template's own id, name and description, with
// no model and no hardcoded keyword table, so new templates match without edits here.

const STOP = new Set([
  'a', 'an', 'and', 'any', 'are', 'as', 'at', 'be', 'but', 'by', 'can', 'do',
  'for', 'from', 'get', 'give', 'has', 'have', 'how', 'i', 'if', 'in', 'is',
  'it', 'its', 'just', 'make', 'me', 'my', 'need', 'new', 'of', 'on', 'or',
  'our', 'out', 'run', 'so', 'some', 'that', 'the', 'their', 'them', 'then',
  'there', 'they', 'this', 'to', 'up', 'use', 'using', 'want', 'was', 'we',
  'what', 'when', 'which', 'will', 'with', 'you', 'your',
  // Domain words in nearly every template description; they separate nothing.
  'sandbox', 'sandboxes', 'server', 'app', 'code', 'file', 'files', 'project',
])

export function words(s: string): string[] {
  return s
    .toLowerCase()
    // Hyphens split so "web-scraper" is reachable by "scrape"; dots survive so "3.11" stays intact.
    .replace(/[^a-z0-9\s.+#]/g, ' ')
    .split(/\s+/)
    .filter(w => w.length > 1 && !STOP.has(w))
}

/** stem collapses inflections; applied to both sides, so it only has to be self-consistent. */
export function stem(w: string): string {
  return w.replace(/(ings|ing|ers|er|es|s)$/, '').replace(/e$/, '')
}

/** offByOne reports one substitution, insertion, deletion, or adjacent transposition apart. */
export function offByOne(a: string, b: string): boolean {
  if (a === b) return true
  const la = a.length
  const lb = b.length
  if (Math.abs(la - lb) > 1) return false
  if (la === lb) {
    let i = 0
    while (i < la && a[i] === b[i]) i++
    if (i === la) return true
    if (a[i] === b[i + 1] && a[i + 1] === b[i]) return a.slice(i + 2) === b.slice(i + 2)
    return a.slice(i + 1) === b.slice(i + 1)
  }
  const [short, long] = la < lb ? [a, b] : [b, a]
  let i = 0
  while (i < short.length && short[i] === long[i]) i++
  return short.slice(i) === long.slice(i + 1)
}

// Exact, a shared five-char prefix, or one typo apart for six-plus letters (at five,
// "cache" and "catch" collide).
function related(a: string, b: string): boolean {
  if (a === b) return true
  const min = Math.min(a.length, b.length)
  if (min >= 5 && a.startsWith(b.slice(0, 5)) && b.startsWith(a.slice(0, 5))) return true
  return min >= 6 && offByOne(a, b)
}

export interface IntentMatch {
  template: Template
  score: number
}

export interface IntentResult {
  best: Template | null
  /** Alternatives to offer when best is null, best first. */
  candidates: Template[]
}

export function scoreTemplate(qs: Set<string>, t: Template): number {
  // An id or name hit is a far stronger signal than a word in a description.
  const idw = words(`${t.id} ${t.name}`).map(stem)
  const dw = words(t.description ?? '').map(stem)
  let score = 0
  for (const q of qs) {
    if (idw.some(w => related(w, q))) score += 3
    else if (dw.some(w => related(w, q))) score += 1
  }
  return score
}

export function queryTerms(text: string): Set<string> {
  return new Set(words(text).map(stem))
}

/** matchIntent returns a confident best only when one template clearly wins; a near-tie returns candidates. */
export function matchIntent(text: string, catalog: Template[], exclude: Set<string> = new Set()): IntentResult {
  const qs = new Set(words(text).map(stem))
  if (qs.size === 0) return { best: null, candidates: [] }

  const scored: IntentMatch[] = catalog
    .filter(t => !exclude.has(t.id))
    .map(t => ({ template: t, score: scoreTemplate(qs, t) }))
    .filter(m => m.score > 0)
    .sort((a, b) => b.score - a.score || a.template.id.localeCompare(b.template.id))

  if (scored.length === 0) return { best: null, candidates: [] }

  const top = scored[0]
  const runnerUp = scored[1]

  // 3 means an id/name hit or several description hits; one weak hit is incidental.
  if (top.score < 3) return { best: null, candidates: [] }

  // A winner ahead by a single weak hit is a coin flip, not a ranking.
  const decisive = !runnerUp || top.score - runnerUp.score >= 2
  if (decisive) return { best: top.template, candidates: [] }

  return { best: null, candidates: scored.slice(0, 3).map(m => m.template) }
}

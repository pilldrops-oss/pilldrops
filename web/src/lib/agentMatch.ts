import type { Template } from '../types'
import { offByOne } from './intentMatch'

// Matches free text to the agent template it names, with no model call. Returns null
// rather than guessing: a wrong sandbox reads as the product being broken.

function agentsIn(catalog: Template[]): Template[] {
  return catalog.filter(t => !!t.agent)
}

function normalise(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9\s-]/g, ' ').replace(/\s+/g, ' ').trim()
}

/** Whole-word containment: "amp" must not match inside "amplifier". */
function containsWord(haystack: string, needle: string): boolean {
  if (!needle) return false
  return new RegExp(`(?:^|\\s)${needle.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?:\\s|$)`)
    .test(haystack)
}

// One typo is tolerated only for tokens of five-plus characters, so "grow" cannot summon "grok".
function fuzzyContains(qWords: string[], candidate: string): boolean {
  const tokens = candidate.split(' ')
  return tokens.every(tok =>
    qWords.some(w => w === tok || (tok.length >= 5 && w.length >= 4 && offByOne(w, tok))),
  )
}

export function matchAgent(text: string, catalog: Template[]): Template | null {
  const q = normalise(text)
  if (!q) return null
  const qWords = q.split(' ')

  let best: Template | null = null
  let bestLen = 0
  for (const t of agentsIn(catalog)) {
    for (const candidate of [normalise(t.id), normalise(t.name)]) {
      // Names under 3 chars ("go", or "C#" once stripped to "c") false-positive on ordinary prose.
      if (candidate.length < 3) continue
      // Strict '>': on a length tie the first agent in the catalogue wins.
      if (candidate.length > bestLen && (containsWord(q, candidate) || fuzzyContains(qWords, candidate))) {
        best = t
        bestLen = candidate.length
      }
    }
  }
  return best
}

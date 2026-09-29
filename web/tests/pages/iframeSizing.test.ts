import { describe, it, expect } from 'vitest'
import themeCss from '../../src/theme.css?raw'

// An unsized <iframe> renders at the spec default of 300x150 with no warning, so every
// iframe in the app must name a class the stylesheet actually sizes.

// Vite's raw glob rather than node:fs, since the web tsconfig has no node types.
const sources = import.meta.glob('../../src/**/*.tsx', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

// Every className referenced by an <iframe> tag, with its source file.
function iframeClasses(): { file: string; cls: string }[] {
  return Object.entries(sources).flatMap(([file, src]) => {
    const tags = src.match(/<iframe[\s\S]*?\/?>/g) ?? []
    return tags.flatMap(tag => {
      const m = tag.match(/className="([^"]+)"/)
      if (!m) return [{ file, cls: '' }] // no class at all, still a failure
      return m[1].split(/\s+/).filter(Boolean).map(cls => ({ file, cls }))
    })
  })
}

// Declarations from every rule whose selector list mentions .cls; `.a, .b {}` sizes both.
function declarationsFor(css: string, cls: string): string {
  const rules = css.match(/[^{}]+\{[^{}]*\}/g) ?? []
  const token = new RegExp(`\\.${cls}(?![\\w-])`)
  return rules
    .filter(rule => token.test(rule.slice(0, rule.indexOf('{'))))
    .map(rule => rule.slice(rule.indexOf('{') + 1, -1))
    .join(';')
}

describe('iframe sizing', () => {
  const found = iframeClasses()

  it('finds the app iframes to check', () => {
    // Guards against the scanner passing vacuously.
    expect(found.length).toBeGreaterThan(0)
  })

  it('reads the real stylesheet', () => {
    // Vitest stubs CSS imports to '' unless css: true is set, which would make the checks vacuous.
    expect(themeCss.length).toBeGreaterThan(1000)
    expect(themeCss).toContain('.preview-iframe')
  })

  it.each(found)('$cls sizes its iframe', ({ file, cls }) => {
    expect(cls, `<iframe> in ${file} has no className to size it`).not.toBe('')
    const decls = declarationsFor(themeCss, cls)
    expect(decls, `.${cls} (used by an <iframe> in ${file}) has no rule in theme.css`).not.toBe('')
    expect(decls, `.${cls} does not set a width — the iframe will be 300px wide`).toMatch(/(^|;)\s*width\s*:/)
    expect(decls, `.${cls} does not set a height — the iframe will be 150px tall`).toMatch(/(^|;)\s*height\s*:/)
  })
})

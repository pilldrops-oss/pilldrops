import { describe, it, expect } from 'vitest'
import themeCss from '../../src/theme.css?raw'

// Every routed page needs a root frame that sets padding, or it renders flush against the
// sidebar. Any class works so long as the stylesheet gives it padding.

const sources = import.meta.glob('../../src/pages/*.tsx', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

// Class lists of every root element the page component returns, one per return. Scoped to
// the default export's body so helper components above or below it are not matched.
function rootClassLists(src: string): string[][] {
  const start = src.search(/export default function/)
  if (start < 0) return []
  const rest = src.slice(start)
  // Top-level functions close with a brace in column 0.
  const end = rest.search(/\n\}\s*\n/)
  const body = end > 0 ? rest.slice(0, end) : rest

  const lists: string[][] = []
  // A root with no className must yield an empty list, not no match; that is the defect to catch.
  for (const m of withoutNestedFunctions(body).matchAll(/return \(?\s*(?:\/\/[^\n]*\n\s*)*<div([^>]*)>/g)) {
    const attrs = m[1] ?? ''
    const cls = attrs.match(/className=(?:"([^"]*)"|\{`([^`]*)`)/)
    const raw = cls ? (cls[1] ?? cls[2] ?? '') : ''
    lists.push(raw.split(/[\s${}]+/).filter(c => /^[a-z][a-z0-9-]*$/.test(c)))
  }
  return lists
}

// Local helper functions inside a page component return fragments of the page,
// not its root, so their returns must not be treated as roots. Drops each
// `  function name(...) {...}` block declared at the component's own indent.
function withoutNestedFunctions(body: string): string {
  let out = ''
  let i = 0
  const decl = /\n {2}function [A-Za-z_$][\w$]*\s*\(/g
  let m: RegExpExecArray | null
  while ((m = decl.exec(body)) !== null) {
    if (m.index < i) continue
    const open = body.indexOf('{', m.index)
    if (open < 0) break
    let depth = 0
    let j = open
    for (; j < body.length; j++) {
      if (body[j] === '{') depth++
      else if (body[j] === '}' && --depth === 0) break
    }
    out += body.slice(i, m.index)
    i = j + 1
    decl.lastIndex = i
  }
  return out + body.slice(i)
}

function setsPadding(cls: string): boolean {
  const rules = themeCss.match(/[^{}]+\{[^{}]*\}/g) ?? []
  const token = new RegExp(`\\.${cls}(?![\\w-])`)
  return rules.some(rule => {
    const [selector, body] = [rule.slice(0, rule.indexOf('{')), rule.slice(rule.indexOf('{') + 1)]
    return token.test(selector) && /(^|;|\s)padding(-inline|-left)?\s*:/.test(body)
  })
}

// Full-height viewers that manage their own chrome.
const UNFRAMED = new Set(['Logs'])

describe('routed pages are framed', () => {
  const pages = Object.entries(sources).filter(([path]) => !path.includes('.test.'))

  it('finds the pages to check', () => {
    // Without this the suite would pass vacuously if the glob broke.
    expect(pages.length).toBeGreaterThan(10)
  })

  it('reads the real stylesheet', () => {
    expect(themeCss).toContain('.wrap')
  })

  it.each(pages.map(([path]) => path))('%s sits in a padded frame', path => {
    const name = path.replace(/^\.\//, '').replace(/\.tsx$/, '')
    if (UNFRAMED.has(name)) return

    const roots = rootClassLists(sources[path])
    if (roots.length === 0) return // root is not a plain <div className=…>; nothing to assert

    for (const classes of roots) {
      expect(
        classes.some(setsPadding),
        `${name} returns <div class="${classes.join(' ')}"> as its root — none of those classes ` +
          `set padding, so the page sits flush against the sidebar with no gutter. Wrap it in ` +
          `<div className="wrap">, or give it its own padded frame class.`,
      ).toBe(true)
    }
  })
})

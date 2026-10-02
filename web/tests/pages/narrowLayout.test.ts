import { describe, it, expect } from 'vitest'
import themeCss from '../../src/theme.css?raw'

// B10: at 400px the sandbox list overflowed an `overflow:hidden` panel (names
// clipped, pills overlapping), and the detail breadcrumb id ran under the
// theme button. Same helper as iframeSizing.test.ts: match every declaration
// block whose selector list mentions the class, @media nesting included.
function declarationsFor(css: string, selector: string): string {
  const rules = css.match(/[^{}]+\{[^{}]*\}/g) ?? []
  const token = new RegExp(`${selector.replace(/[.[\]]/g, '\\$&')}(?![\\w-])`)
  return rules
    .filter(rule => token.test(rule.slice(0, rule.indexOf('{'))))
    .map(rule => rule.slice(rule.indexOf('{') + 1, -1))
    .join(';')
}

describe('400px layout (B10)', () => {
  it('reads the real stylesheet', () => {
    expect(themeCss.length).toBeGreaterThan(1000)
  })

  it('the sandbox list fits the panel: a shrinkable name column, the rest hidden', () => {
    expect(declarationsFor(themeCss, '.sb-table-scroll .row')).toMatch(/grid-template-columns\s*:\s*minmax\(0,\s*1fr\)/)
    expect(themeCss).toMatch(/\.sb-table-scroll \.row > :not\(:first-child\)[^{]*\{\s*display\s*:\s*none/)
  })

  it('the list toolbar wraps instead of running off the panel', () => {
    expect(declarationsFor(themeCss, '.sb-ph .rgt')).toMatch(/flex-wrap\s*:\s*wrap/)
    expect(declarationsFor(themeCss, '.sb-ph .sb-search')).toMatch(/min-width\s*:\s*0/)
  })

  it('the breadcrumb shrinks instead of overrunning the theme button', () => {
    expect(declarationsFor(themeCss, '.bc ')).toMatch(/min-width\s*:\s*0/)
  })

  it('a long breadcrumb sub-id (sandbox id) ellipsizes instead of overflowing', () => {
    const decls = declarationsFor(themeCss, '.bc-sub')
    expect(decls).toMatch(/overflow\s*:\s*hidden/)
    expect(decls).toMatch(/text-overflow\s*:\s*ellipsis/)
    expect(decls).toMatch(/white-space\s*:\s*nowrap/)
  })
})

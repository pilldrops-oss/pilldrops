import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import { TemplateIcon } from '../../src/components/templateIcons'

// B19: go, rust and playwright used to fall back to two-letter text badges.
describe('TemplateIcon (B19)', () => {
  it('go gets a glyph, not the GO text fallback', () => {
    const { container } = render(<TemplateIcon desc={{ id: 'go', name: 'Go', language: 'go', tags: ['go'] }} />)
    expect(container.querySelector('svg')).not.toBeNull()
    expect(container.textContent).not.toBe('GO')
  })

  it('rust gets a glyph, not the RU text fallback', () => {
    const { container } = render(
      <TemplateIcon desc={{ id: 'rust', name: 'Rust', language: 'rust', tags: ['rust', 'cargo'] }} />,
    )
    expect(container.querySelector('svg')).not.toBeNull()
    expect(container.textContent).not.toBe('RU')
  })

  it('playwright gets its own glyph, not the generic browser/chrome icon', () => {
    const { container: pw } = render(
      <TemplateIcon
        desc={{
          id: 'playwright', name: 'Playwright (E2E testing)', language: 'node',
          tags: ['browser', 'testing', 'e2e', 'recording'],
        }}
      />,
    )
    const { container: browser } = render(
      <TemplateIcon desc={{ id: 'browser', name: 'Browser', language: 'node', tags: ['browser', 'computer-use', 'gui'] }} />,
    )
    expect(pw.querySelector('svg')).not.toBeNull()
    expect(pw.innerHTML).not.toBe(browser.innerHTML)
  })
})

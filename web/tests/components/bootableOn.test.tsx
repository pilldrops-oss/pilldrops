import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { TemplateGallery, bootableOn } from '../../src/components/launch/TemplateGallery'
import type { Template } from '../../src/types'

const tpl = (id: string, available?: boolean): Template =>
  ({ id, name: id, description: '', cpu: 1, memory_mb: 512, visibility: 'public', available })

// The server computes `available` from the scheduler's own rule
// (cluster.Node.hostsTemplate); the web just reads it.
describe('bootableOn', () => {
  it('boots what the catalog marks available, nothing else', () => {
    const ok = bootableOn([tpl('base', true), tpl('rust', false)])
    expect(ok('base')).toBe(true)
    expect(ok('rust')).toBe(false)
  })

  it('treats a missing `available` as available, same as an unknown id', () => {
    expect(bootableOn([tpl('desktop')])('desktop')).toBe(true)
    expect(bootableOn([tpl('desktop', true)])('anything-else')).toBe(true)
  })

  it('greys nothing when there is no catalog', () => {
    expect(bootableOn([])('anything')).toBe(true)
  })
})

describe('gallery', () => {
  const tpl = (id: string): Template => ({ id, name: id, description: '', cpu: 1, memory_mb: 512, visibility: 'public' })

  it('greys a card no node can boot and offers it no quick launch', () => {
    const onPick = vi.fn()
    const { container } = render(
      <MemoryRouter>
        <TemplateGallery templates={[tpl('base'), tpl('browser')]} onPick={onPick} onQuickLaunch={vi.fn()} bootable={id => id === 'base'} />
      </MemoryRouter>,
    )
    const off = container.querySelectorAll('.dep-card.off')
    expect(off).toHaveLength(1)
    expect(off[0].textContent).toContain('browser')
    expect(off[0].getAttribute('aria-disabled')).toBe('true')
    expect(off[0].getAttribute('tabindex')).toBe('-1')
    expect(screen.getAllByText(/Quick launch/)).toHaveLength(1)
    ;(off[0] as HTMLElement).click()
    expect(onPick).not.toHaveBeenCalled()
  })
})

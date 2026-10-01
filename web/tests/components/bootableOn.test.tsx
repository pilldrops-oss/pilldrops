import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { TemplateGallery, bootableOn } from '../../src/components/launch/TemplateGallery'
import type { NodeInfo, Template } from '../../src/types'

const node = (over: Partial<NodeInfo>): NodeInfo => ({
  id: 'n', url: '', zone: '', healthy: true, status: 'ok', first_seen: 0, last_seen: 0,
  capacity: {
    cpu_cores: 1, cpu_percent: 0, load1: 0, mem_total_mb: 0, mem_available_mb: 0,
    mem_committed_mb: 0, disk_free_gb: 0, sandboxes_active: 0,
  },
  ...over,
})

// The same rule the scheduler applies (cluster.Node.hostsTemplate).
describe('bootableOn', () => {
  it('boots what a healthy node holds, nothing else', () => {
    const ok = bootableOn([node({ templates: ['base'] }), node({ templates: ['rust'], healthy: false })])
    expect(ok('base')).toBe(true)
    expect(ok('rust')).toBe(false)
  })

  it('treats a node that does not enumerate, or can fetch, as booting anything', () => {
    expect(bootableOn([node({ templates: null })])('desktop')).toBe(true)
    expect(bootableOn([node({})])('desktop')).toBe(true)
    expect(bootableOn([node({ templates: [], can_fetch_templates: true })])('desktop')).toBe(true)
  })

  it('greys nothing when there is no node list', () => {
    expect(bootableOn(null)('anything')).toBe(true)
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

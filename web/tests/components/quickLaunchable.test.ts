import { describe, it, expect } from 'vitest'
import { quickLaunchable } from '../../src/components/launchConfig'
import type { Template } from '../../src/types'

const plain = (over: Partial<Template> = {}): Template => ({
  id: 'base', name: 'Base', description: '', tags: [], language: 'shell',
  cpu: 1, memory_mb: 512, visibility: 'public',
  ...over,
} as Template)

describe('quickLaunchable', () => {
  it('still allows a zero-input template', () => {
    expect(quickLaunchable(plain())).toBe(true)
  })

  it('still blocks a required non-secret env with no default', () => {
    expect(
      quickLaunchable(plain({ env: [{ name: 'TARGET_URL', required: true }] })),
    ).toBe(false)
  })

  it('still blocks a required primary input', () => {
    expect(
      quickLaunchable(plain({ primary_input: { type: 'url', label: 'URL', required: true } })),
    ).toBe(false)
  })
})

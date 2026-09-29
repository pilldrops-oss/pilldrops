import { describe, expect, it } from 'vitest'
import { shortVersion } from '../../../src/components/agent/AgentPanel'

// `claude --version` prints "2.1.197 (Claude Code)"; the header already shows the name.
describe('shortVersion', () => {
  it('drops a parenthetical that repeats the product name', () => {
    expect(shortVersion('2.1.197 (Claude Code)')).toBe('2.1.197')
  })

  it('leaves a plain version alone', () => {
    expect(shortVersion('0.147.0')).toBe('0.147.0')
  })

  it('does not mangle a version that merely contains a bracket-free suffix', () => {
    expect(shortVersion('1.18.15-beta.2')).toBe('1.18.15-beta.2')
  })

  it('keeps something rather than emptying the field', () => {
    // A leading parenthesis would otherwise slice to an empty string.
    expect(shortVersion('(unknown)')).toBe('(unknown)')
  })
})

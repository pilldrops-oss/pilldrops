import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { ModeLadder, defaultModeFor } from '../../../src/components/agent/ModeLadder'

// Offering Guided for an agent with no verified approval hook means an ungated
// run under a screen that claims review, so gateability must come from the manifest.

describe('ModeLadder', () => {
  it('disables Guided, with a reason, for an agent that cannot be gated', () => {
    render(<ModeLadder agent={{ key_env: 'X' }} value="autonomous" onChange={() => {}} />)

    const guided = screen.getByRole('radio', { name: /Guided/ })
    expect((guided as HTMLInputElement).disabled).toBe(true)
    expect(screen.getByText(/no approval hook we have verified/)).toBeTruthy()
  })

  it('offers Guided when the manifest carries a verified permission hook', () => {
    render(<ModeLadder
      agent={{ permission_tool: '--permission-prompt-tool {tool}' }}
      value="guided" onChange={() => {}} />)

    const guided = screen.getByRole('radio', { name: /Guided/ })
    expect((guided as HTMLInputElement).disabled).toBe(false)
  })

  it('warns that a Guided launch is contingent on the gate starting', () => {
    render(<ModeLadder
      agent={{ permission_tool: '--x {tool}' }}
      value="guided" onChange={() => {}} />)
    expect(screen.getByText(/cannot start/)).toBeTruthy()
  })

  it('orders the rungs narrowest first', () => {
    render(<ModeLadder agent={{ permission_tool: '--x' }} value="guided" onChange={() => {}} />)
    const labels = screen.getAllByRole('radio').map(r => (r as HTMLInputElement).value)
    expect(labels).toEqual(['supervised', 'guided', 'autonomous'])
  })

  it('reports the chosen rung', async () => {
    const onChange = vi.fn()
    render(<ModeLadder agent={{ permission_tool: '--x' }} value="autonomous" onChange={onChange} />)
    await userEvent.click(screen.getByRole('radio', { name: /Supervised/ }))
    expect(onChange).toHaveBeenCalledWith('supervised')
  })

  it('cannot select a disabled rung', async () => {
    const onChange = vi.fn()
    render(<ModeLadder agent={{}} value="autonomous" onChange={onChange} />)
    await userEvent.click(screen.getByRole('radio', { name: /Guided/ }))
    expect(onChange).not.toHaveBeenCalled()
  })
})

describe('defaultModeFor', () => {
  it('pre-selects the reviewed posture when the agent can be gated', () => {
    expect(defaultModeFor({ permission_tool: '--x' })).toBe('guided')
  })

  it('falls back to autonomous only when review is impossible', () => {
    expect(defaultModeFor({ key_env: 'X' })).toBe('autonomous')
    expect(defaultModeFor(undefined)).toBe('autonomous')
  })
})

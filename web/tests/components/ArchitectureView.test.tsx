import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import ArchitectureView from '../../src/components/ArchitectureView'
import type { Plan } from '../../src/lib/plan'

const plan: Plan = {
  services: [
    {
      id: 'api',
      role: 'app',
      template: { id: 'python-3.11', name: 'Python 3.11', kind: 'runtime', tags: ['python'], port: 8000 } as never,
      reason: 'you asked for a python api',
    },
  ],
  links: [],
  gaps: [],
  confidence: 'exact',
}

describe('node status', () => {
  it('draws an unlaunched plan as a proposal', () => {
    render(<ArchitectureView plan={plan} phase="proposed" />)
    expect(screen.getByText(/proposed/)).toBeTruthy()
  })

  it('says "starting" only while a launch is actually in flight', () => {
    render(<ArchitectureView plan={plan} phase="live" verdicts={{}} />)
    expect(screen.getByText(/starting/)).toBeTruthy()
  })

  it('does not claim a finished run is still starting', () => {
    render(<ArchitectureView plan={plan} phase="past" verdicts={{}} />)
    expect(screen.queryByText(/starting/)).toBeNull()
    expect(screen.getByText(/not recorded/)).toBeTruthy()
  })

  it('still reports a recorded verdict from a past run', () => {
    render(
      <ArchitectureView
        plan={plan}
        phase="past"
        verdicts={{ api: { verdict: 'serving' } as never }}
      />,
    )
    expect(screen.getByText(/serving/)).toBeTruthy()
  })
})

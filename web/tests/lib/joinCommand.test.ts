import { describe, it, expect } from 'vitest'
import { orchestratorUrlFrom } from '../../src/lib/joinCommand'

describe('orchestratorUrlFrom (B26)', () => {
  it('pulls the ORCHESTRATOR_URL value out of the bootstrap command', () => {
    const cmd = 'curl -fsSL http://localhost:19090/install.sh -o sf-install.sh && ' +
      'ORCHESTRATOR_URL=http://localhost:19090 sh sf-install.sh'
    expect(orchestratorUrlFrom(cmd)).toBe('http://localhost:19090')
  })

  it('returns null when the command has no ORCHESTRATOR_URL', () => {
    expect(orchestratorUrlFrom('echo hi')).toBeNull()
  })
})

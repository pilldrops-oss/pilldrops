import { describe, it, expect } from 'vitest'
import { cpuReading, memReading, diskReading } from '../../src/lib/liveMetrics'
import type { SandboxMetrics } from '../../src/types'

const m = (o: Partial<SandboxMetrics>): SandboxMetrics =>
  ({ id: 'sb-1', state: 'running', vcpu: 1, mem_mib: 1024, vcpu_seconds: 0, ram_gb_seconds: 0, uptime_seconds: 0, ...o })

// vcpu_seconds and ram_gb_seconds are billing accumulators that accrue at the allocated
// rate, so dividing either by uptime returns the box's own size, never utilisation.
describe('cpuReading', () => {
  it('reports utilisation when the platform sends a real instantaneous reading', () => {
    expect(cpuReading(m({ cpu_pct: 42, vcpu_seconds: 10, uptime_seconds: 100 })))
      .toEqual({ text: '42%', kind: 'utilisation' })
  })

  it('reports ALLOCATION, not utilisation, from cumulative billing counters', () => {
    const r = cpuReading(m({ vcpu: 2, vcpu_seconds: 120.006, uptime_seconds: 60 }))
    expect(r).toEqual({ text: '2 vCPU', kind: 'allocation' })
    expect(r!.text).not.toContain('%')
  })

  it('prefers the declared size over re-deriving it', () => {
    expect(cpuReading(m({ vcpu: 4, vcpu_seconds: 0, uptime_seconds: 0 })))
      .toEqual({ text: '4 vCPU', kind: 'allocation' })
  })

  it('says nothing when there is neither a reading nor a size', () => {
    expect(cpuReading(m({ vcpu: 0, vcpu_seconds: 0, uptime_seconds: 0 }))).toBeNull()
    expect(cpuReading(null)).toBeNull()
  })
})

describe('memReading', () => {
  it('reports utilisation when the platform sends real resident memory', () => {
    expect(memReading(m({ mem_used_mib: 512, mem_mib: 1024 })))
      .toEqual({ text: '512 / 1024 MiB', kind: 'utilisation' })
  })

  it('reports ALLOCATION, not utilisation, from cumulative billing counters', () => {
    const r = memReading(m({ ram_gb_seconds: 60, uptime_seconds: 60, mem_mib: 1024 }))
    expect(r).toEqual({ text: '1024 MiB', kind: 'allocation' })
    expect(r!.text).not.toContain('avg')
  })

  it('says nothing when there is neither a reading nor a size', () => {
    expect(memReading(m({ mem_mib: 0, ram_gb_seconds: 5, uptime_seconds: 0 }))).toBeNull()
    expect(memReading(null)).toBeNull()
  })
})

describe('diskReading', () => {
  it('reports real usage when the platform sends it', () => {
    expect(diskReading(m({ disk_used_mb: 120, disk_limit_mb: 2048 })))
      .toEqual({ text: '120 / 2048 MB', kind: 'utilisation' })
  })

  it('says nothing when there is no way to know', () => {
    expect(diskReading(m({}))).toBeNull()
  })
})

import type { SandboxMetrics } from '../types'

// `vcpu_seconds` and `ram_gb_seconds` are billing accumulators that accrue at the
// allocated rate, so dividing by uptime yields the box's size, never its utilisation.
// A Reading therefore says which of the two it is, and callers must label them differently.

export interface Reading {
  text: string
  kind: 'utilisation' | 'allocation'
}

const MIB_PER_GB = 1024

export function cpuReading(m: SandboxMetrics | null | undefined): Reading | null {
  if (!m) return null
  if (typeof m.cpu_pct === 'number') return { text: `${Math.round(m.cpu_pct)}%`, kind: 'utilisation' }

  // Cores, not a percentage: a percent sign reads as effort rather than size.
  const cores = m.vcpu || derivedCores(m)
  if (!cores) return null
  return { text: `${cores} vCPU`, kind: 'allocation' }
}

// Allocated cores recovered from the billing counter; exact because it accrues at the allocated rate.
function derivedCores(m: SandboxMetrics): number {
  if (!m.uptime_seconds || !Number.isFinite(m.vcpu_seconds)) return 0
  return Math.round(m.vcpu_seconds / m.uptime_seconds)
}

export function memReading(m: SandboxMetrics | null | undefined): Reading | null {
  if (!m) return null
  if (typeof m.mem_used_mib === 'number') {
    const cap = m.mem_mib ? ` / ${m.mem_mib} MiB` : ' MiB'
    return { text: `${m.mem_used_mib}${cap}`, kind: 'utilisation' }
  }

  // Shown bare, not as "used of total", since this is the box's size.
  const mib = m.mem_mib || derivedMib(m)
  if (!mib) return null
  return { text: `${mib} MiB`, kind: 'allocation' }
}

function derivedMib(m: SandboxMetrics): number {
  if (!m.uptime_seconds || !Number.isFinite(m.ram_gb_seconds)) return 0
  return Math.round((m.ram_gb_seconds / m.uptime_seconds) * MIB_PER_GB)
}

export function diskReading(m: SandboxMetrics | null | undefined): Reading | null {
  if (!m || typeof m.disk_used_mb !== 'number') return null
  return {
    text: `${m.disk_used_mb}${m.disk_limit_mb ? ` / ${m.disk_limit_mb} MB` : ''}`,
    kind: 'utilisation',
  }
}

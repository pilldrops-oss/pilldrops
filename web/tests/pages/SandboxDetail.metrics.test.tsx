import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, cleanup } from '@testing-library/react'
import { MetricsStrip } from '../../src/pages/SandboxDetail'
import { getSandboxMetrics } from '../../src/api'
import type { SandboxMetrics } from '../../src/types'

vi.mock('../../src/api', () => ({
  getSandboxMetrics: vi.fn(),
}))

const getMetricsMock = vi.mocked(getSandboxMetrics)

function metrics(overrides: Partial<SandboxMetrics> = {}): SandboxMetrics {
  return {
    id: 'sb1',
    state: 'running',
    vcpu: 2,
    mem_mib: 1024,
    vcpu_seconds: 10,
    ram_gb_seconds: 1,
    uptime_seconds: 125,
    cpu_pct: 12.5,
    mem_used_mib: 300,
    disk_used_mb: 40,
    disk_limit_mb: 8192,
    ...overrides,
  }
}

describe('MetricsStrip', () => {
  beforeEach(() => {
    getMetricsMock.mockReset()
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] })
  })
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it('renders CPU, memory (against its cap), and disk', async () => {
    getMetricsMock.mockResolvedValue(metrics())
    render(<MetricsStrip id="sb1" status="running" />)

    await waitFor(() => expect(screen.getByTestId('metric-cpu')).toHaveTextContent('13%'))
    expect(screen.getByTestId('metric-mem')).toHaveTextContent('300')
    expect(screen.getByTestId('metric-mem')).toHaveTextContent('1024')
    expect(screen.getByTestId('metric-disk')).toHaveTextContent('40')
  })

  it('raises a warning affordance when memory crosses ~85% of its cap', async () => {
    getMetricsMock.mockResolvedValue(metrics({ mem_used_mib: 900, mem_mib: 1024 }))
    render(<MetricsStrip id="sb1" status="running" />)

    await waitFor(() => expect(screen.getByTestId('metric-mem-warning')).toBeInTheDocument())
  })

  it('does not warn when memory is comfortably under the cap', async () => {
    getMetricsMock.mockResolvedValue(metrics({ mem_used_mib: 300, mem_mib: 1024 }))
    render(<MetricsStrip id="sb1" status="running" />)

    await waitFor(() => expect(screen.getByTestId('metric-cpu')).toHaveTextContent('13%'))
    expect(screen.queryByTestId('metric-mem-warning')).toBeNull()
  })

  // vcpu_seconds and ram_gb_seconds are billing accumulators that accrue at the allocated
  // rate, so without a live reading the tile reports allocation, labelled as such.
  it('reports allocation, clearly labelled, when utilisation is unavailable', async () => {
    getMetricsMock.mockResolvedValue(
      metrics({
        cpu_pct: undefined, mem_used_mib: undefined, disk_used_mb: undefined,
        vcpu: 2, vcpu_seconds: 120, ram_gb_seconds: 60, uptime_seconds: 60, mem_mib: 1024,
      }),
    )
    render(<MetricsStrip id="sb1" status="running" />)

    await waitFor(() => expect(screen.getByTestId('metric-cpu')).toHaveTextContent('2 vCPU'))
    expect(screen.getByTestId('metric-cpu')).not.toHaveTextContent('%')
    expect(screen.getByTestId('metric-cpu')).toHaveTextContent('CPU size')
    expect(screen.getByTestId('metric-mem')).toHaveTextContent('1024 MiB')
    expect(screen.getByTestId('metric-mem')).toHaveTextContent('Memory size')
    expect(screen.queryByTestId('metric-disk')).toBeNull()
  })

  it('hides a tile entirely when nothing can be derived for it', async () => {
    getMetricsMock.mockResolvedValue(
      metrics({
        cpu_pct: undefined, mem_used_mib: undefined, disk_used_mb: undefined,
        vcpu: 0, mem_mib: 0, vcpu_seconds: 0, ram_gb_seconds: 0, uptime_seconds: 0,
      }),
    )
    render(<MetricsStrip id="sb1" status="running" />)
    await waitFor(() => expect(screen.queryByTestId('metric-disk')).toBeNull())
    expect(screen.queryByTestId('metric-cpu')).toBeNull()
    expect(screen.queryByTestId('metric-mem')).toBeNull()
  })

  it('renders nothing for a paused sandbox', () => {
    const { container } = render(<MetricsStrip id="sb1" status="paused" />)
    expect(container).toBeEmptyDOMElement()
    expect(getMetricsMock).not.toHaveBeenCalled()
  })

  it('stops polling once unmounted', async () => {
    getMetricsMock.mockResolvedValue(metrics())
    const { unmount } = render(<MetricsStrip id="sb1" status="running" />)
    await waitFor(() => expect(getMetricsMock).toHaveBeenCalledTimes(1))

    unmount()
    const callsAtUnmount = getMetricsMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(30_000)
    expect(getMetricsMock.mock.calls.length).toBe(callsAtUnmount)
  })
})

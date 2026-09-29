import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import PreviewDiagnosis from '../../src/components/PreviewDiagnosis'

describe('PreviewDiagnosis', () => {
  it('explains a timeout and offers the terminal', async () => {
    const onOpen = vi.fn()
    render(
      <PreviewDiagnosis
        outcome={{ state: 'unreachable', lastState: 'starting' }}
        sandboxId="sb1"
        onOpen={onOpen}
      />,
    )
    expect(screen.getByText(/didn’t start listening in time/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /open the terminal/i }))
    expect(onOpen).toHaveBeenCalledWith('sb1')
  })

  it('distinguishes a 404 from a dead app', () => {
    render(
      <PreviewDiagnosis
        outcome={{ state: 'no_page', lastState: 'no_page', upstreamStatus: 404 }}
        sandboxId="sb1"
      />,
    )
    expect(screen.getByText(/running, but there’s no page at/i)).toBeInTheDocument()
  })

  it('reports an app-side 5xx as the app’s own error', () => {
    render(
      <PreviewDiagnosis
        outcome={{ state: 'app_error', lastState: 'app_error', upstreamStatus: 500 }}
        sandboxId="sb1"
      />,
    )
    expect(screen.getByText(/returned an error \(500\)/i)).toBeInTheDocument()
  })

  it('shows the deploy log when one is available', () => {
    render(
      <PreviewDiagnosis
        outcome={{ state: 'unreachable', lastState: 'starting' }}
        sandboxId="sb1"
        deployLog="npm ERR! missing script: start"
      />,
    )
    expect(screen.getByText(/npm ERR! missing script/)).toBeInTheDocument()
  })

  it('renders nothing when the preview is ready', () => {
    const { container } = render(
      <PreviewDiagnosis outcome={{ state: 'ready', lastState: 'ready' }} sandboxId="sb1" />,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when readiness is unknown', () => {
    const { container } = render(
      <PreviewDiagnosis outcome={{ state: 'unknown' }} sandboxId="sb1" />,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('says plainly when the preview is a terminal, not the app', () => {
    render(
      <PreviewDiagnosis
        outcome={{ state: 'terminal', lastState: 'terminal' }}
        sandboxId="sb1"
      />,
    )
    expect(screen.getByText(/terminal, not your app/i)).toBeInTheDocument()
  })
})

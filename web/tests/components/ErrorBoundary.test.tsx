import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { useState } from 'react'
import ErrorBoundary from '../../src/components/ErrorBoundary'

// A page that throws during render must cost that page and nothing else.

function Boom({ msg = 'kaboom' }: { msg?: string }): React.ReactElement {
  throw new Error(msg)
}

describe('ErrorBoundary', () => {
  // React logs caught errors to console.error; silence the deliberate throws.
  beforeEach(() => vi.spyOn(console, 'error').mockImplementation(() => {}))
  afterEach(() => vi.restoreAllMocks())

  it('renders children when nothing throws', () => {
    render(<ErrorBoundary><p>the page</p></ErrorBoundary>)
    expect(screen.getByText('the page')).toBeTruthy()
  })

  it('shows the failure instead of unmounting, and surfaces the message', () => {
    render(<ErrorBoundary><Boom msg="last_used.startsWith is not a function" /></ErrorBoundary>)
    expect(screen.getByText(/This page hit an error/)).toBeTruthy()
    expect(screen.getByText(/startsWith is not a function/)).toBeTruthy()
  })

  it('leaves the surrounding shell mounted', () => {
    render(
      <div>
        <nav>sidebar</nav>
        <ErrorBoundary><Boom /></ErrorBoundary>
      </div>,
    )
    expect(screen.getByText('sidebar')).toBeTruthy()
  })

  it('clears on navigation so the error does not latch across pages', () => {
    function Harness() {
      const [path, setPath] = useState('/keys')
      return (
        <div>
          <button onClick={() => setPath('/usage')}>go</button>
          <ErrorBoundary resetKey={path}>
            {path === '/keys' ? <Boom /> : <p>usage page</p>}
          </ErrorBoundary>
        </div>
      )
    }
    render(<Harness />)
    expect(screen.getByText(/This page hit an error/)).toBeTruthy()
    fireEvent.click(screen.getByText('go'))
    expect(screen.getByText('usage page')).toBeTruthy()
    expect(screen.queryByText(/This page hit an error/)).toBeNull()
  })
})

import { Component } from 'react'
import type { ErrorInfo, ReactNode } from 'react'

type Props = { children: ReactNode; resetKey?: string }
type State = { err: Error | null; key?: string }

export default class ErrorBoundary extends Component<Props, State> {
  state: State = { err: null }

  static getDerivedStateFromError(err: Error): Partial<State> {
    return { err }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (state.key !== props.resetKey) {
      return { err: null, key: props.resetKey }
    }
    return null
  }

  componentDidCatch(err: Error, info: ErrorInfo) {
    console.error('page crashed:', err, info.componentStack)
  }

  render() {
    const { err } = this.state
    if (!err) return this.props.children
    return (
      <div className="callout error" style={{ maxWidth: 640, margin: '32px auto' }}>
        <strong style={{ color: 'var(--red)' }}>This page hit an error</strong>
        <p style={{ margin: '8px 0 12px', color: 'var(--sub)', fontSize: 13 }}>
          The rest of the dashboard still works — pick another tab, or reload to try again.
        </p>
        <code style={{
          display: 'block',
          fontFamily: 'var(--mono)',
          fontSize: 12,
          background: 'var(--surface)',
          border: '1px solid var(--line)',
          borderRadius: 8,
          padding: '8px 12px',
          wordBreak: 'break-word',
          color: 'var(--ink)',
        }}>
          {err.message || String(err)}
        </code>
        <button className="btn" style={{ marginTop: 12 }} onClick={() => this.setState({ err: null })}>
          Retry
        </button>
      </div>
    )
  }
}

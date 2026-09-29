import { errText } from '../lib/errText'

export default function ErrorState({
  error,
  onRetry,
  title = 'Something went wrong',
}: { error: unknown; onRetry?: () => void; title?: string }) {
  return (
    <div className="callout error error-state" role="alert">
      <div className="error-state-head">
        <b>{title}</b>
        {onRetry && <button className="btn" onClick={onRetry}>Retry</button>}
      </div>
      <p className="error-state-msg">{errText(error)}</p>
    </div>
  )
}

import type { PollResult } from '../lib/previewStatus'

interface Diagnosis {
  headline: string
  detail: string
  action?: 'terminal'
}

function diagnose(outcome: PollResult): Diagnosis | null {
  switch (outcome.state) {
    case 'ready':
      return null
    case 'unknown':
      return null
    case 'unreachable':
      return {
        headline: 'Your app didn’t start listening in time.',
        detail:
          'The sandbox is running, but nothing answered on the preview port. Common causes: the start command exited, dependencies are still installing, or the app is listening on a different port.',
        action: 'terminal',
      }
    case 'no_page':
      return {
        headline: 'Your app is running, but there’s no page at /.',
        detail:
          'Something is serving on this port and it returned 404 for the root path. If your app only serves specific routes, try opening one of them directly.',
      }
    case 'app_error':
      return {
        headline: `Your app started, then returned an error (${outcome.upstreamStatus ?? '5xx'}).`,
        detail:
          'This is your application’s own error, not a sandbox failure — the platform reached it successfully. Check the logs for the stack trace.',
        action: 'terminal',
      }
    case 'terminal':
      return {
        headline: 'The preview is a terminal, not your app.',
        detail:
          'No app was started in this sandbox, so the preview URL serves the built-in web terminal. Run your app from the terminal (bind it to $PORT), or redeploy with a start command.',
        action: 'terminal',
      }
    case 'unpublished':
      return {
        headline: 'This sandbox publishes a different port.',
        detail:
          'The port the preview probed is not the one this sandbox exposes, so nothing can ever answer there. Open the sandbox to see the published port.',
        action: 'terminal',
      }
    case 'paused':
      return {
        headline: 'The sandbox is paused.',
        detail:
          'It will resume on the first real request to its preview URL — status checks never wake it (or bill you).',
      }
    case 'starting':
      return {
        headline: 'Your app is still starting.',
        detail: 'The preview will start working as soon as it binds its port.',
      }
  }
}

export default function PreviewDiagnosis({
  outcome,
  sandboxId,
  onOpen,
  deployLog,
}: {
  outcome: PollResult
  sandboxId: string
  onOpen?: (id: string) => void
  deployLog?: string
}) {
  const d = diagnose(outcome)
  if (!d) return null

  return (
    <div className="dp-diag" role="status">
      <p className="dp-diag-h">{d.headline}</p>
      <p className="dp-diag-d">{d.detail}</p>

      {deployLog && (
        <details className="dp-diag-log">
          <summary>Deploy log</summary>
          <pre>{deployLog}</pre>
        </details>
      )}

      {d.action === 'terminal' && onOpen && (
        <button type="button" className="btn" onClick={() => onOpen(sandboxId)}>
          Open the terminal →
        </button>
      )}
    </div>
  )
}

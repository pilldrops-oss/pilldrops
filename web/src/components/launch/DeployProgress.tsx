import { useState, useEffect, useRef } from 'react'

import type { Template, CreateSandboxResp } from '../../types'

import { usePreviewLink } from '../../lib/previewToken'
import type { PollResult } from '../../lib/previewStatus'
import PreviewDiagnosis from '../PreviewDiagnosis'

import { formatCountdown } from '../../lib/lifetime'

import { type Kind, serviceConn } from '../../lib/launchKinds'

import { ServiceConnBlock } from './panels'

export function deploySteps(opts: {
  kind: Kind
  hasFiles: boolean
  hasSetup: boolean
  hasStart: boolean
}): string[] {
  const { kind, hasFiles, hasSetup, hasStart } = opts
  const steps: string[] = ['Create sandbox']
  if (hasFiles) steps.push('Upload your files')
  if (hasSetup) steps.push('Install dependencies')
  const serves = hasStart || kind === 'gui' || kind === 'service'
  if (serves) {
    steps.push(`Start the ${START_NOUN[kind] ?? 'app'}`)
  }
  steps.push('App reachable')
  return steps
}

const STEP_GLYPH = { done: '✓', pending: '○' } as const

function stepStatus(i: number, active: number): 'done' | 'active' | 'pending' {
  if (i < active) return 'done'
  return i === active ? 'active' : 'pending'
}

const START_NOUN: Partial<Record<Kind, string>> = { service: 'service', gui: 'GUI' }

export type DeployPhaseState = 'running' | 'done' | 'error'

export function DeployProgress({
  steps,
  state,
  verifying = false,
  previewOutcome,
  liveStatus,
  result,
  error,
  onReset,
  onCancel,
  onRetry,
  service,
  onOpen,
}: {
  steps: string[]
  state: DeployPhaseState
  verifying?: boolean
  previewOutcome?: PollResult | null
  liveStatus?: string | null
  result?: CreateSandboxResp | null
  error?: string | null
  onReset: () => void
  onCancel?: () => void
  onRetry?: () => void
  service?: Template | null
  onOpen?: (id: string) => void
}) {
  const [elapsed, setElapsed] = useState(0)
  const lastIndex = steps.length - 1
  const active = verifying ? lastIndex : 0

  useEffect(() => {
    if (state !== 'running') { setElapsed(0); return }
    const start = Date.now()
    const id = window.setInterval(() => setElapsed(Math.floor((Date.now() - start) / 1000)), 1000)
    return () => window.clearInterval(id)
  }, [state])

  if (state === 'error') {
    return (
      <div className="dp-card dp-error">
        <div className="dp-title dp-title-err">Deploy failed</div>
        <p className="dp-errtext">{error || 'Something went wrong. Please try again.'}</p>
        <div className="dp-actions">
          {onRetry && (
            <button type="button" className="btn primary" onClick={onRetry}>
              Retry
            </button>
          )}
          <button type="button" className="btn" onClick={onReset}>
            Back
          </button>
        </div>
      </div>
    )
  }

  if (state === 'done' && result) {
    return (
      <>
        <DeployReady
          result={result}
          previewOutcome={previewOutcome}
          liveStatus={liveStatus}
          onReset={onReset}
          service={service}
          onOpen={onOpen}
        />
      </>
    )
  }

  return (
    <div className="dp-card">
      <div className="dp-title">
        Deploying your sandbox…
        {elapsed > 0 && <span className="dp-elapsed"> · {elapsed}s</span>}
        {onCancel && (
          <button type="button" className="btn dp-cancel" onClick={onCancel}>
            Cancel
          </button>
        )}
      </div>
      <ol className="dp-steps">
        {steps.map((label, i) => {
          const status = stepStatus(i, active)
          return (
            <li key={label} className={`dp-step dp-${status}`}>
              <span className="dp-glyph" aria-hidden="true">
                {status === 'active' ? <span className="dp-spinner" /> : STEP_GLYPH[status]}
              </span>
              <span className="dp-step-label">{label}</span>
            </li>
          )
        })}
      </ol>
      <p className="dp-helper">
        {verifying
          ? 'Sandbox is up — waiting for the preview URL to answer.'
          : 'One create call runs all of these in order server-side — typically 10–60s.'}
      </p>
    </div>
  )
}

type LaunchOutcome = { glyph: string; tone: string; title: string }

function launchOutcome(o: {
  dead: boolean
  liveStatus?: string | null
  isReady: boolean
  isIndeterminate: boolean
}): LaunchOutcome {
  if (o.dead) {
    return {
      glyph: '!',
      tone: 'dp-title-warn',
      title: o.liveStatus === 'crashed'
        ? 'Sandbox crashed — the start command exited'
        : 'Sandbox stopped — its process finished',
    }
  }
  if (o.isReady) return { glyph: '✓', tone: 'dp-title-ok', title: 'Sandbox is ready' }
  if (o.isIndeterminate) return { glyph: '·', tone: 'dp-title-neutral', title: 'Sandbox is running' }
  return {
    glyph: '!',
    tone: 'dp-title-warn',
    title: 'Sandbox is running — the app isn’t answering',
  }
}

function DeployReady({
  result,
  previewOutcome = null,
  liveStatus = null,
  onReset,
  service,
  onOpen,
}: {
  result: CreateSandboxResp
  onReset: () => void
  service?: Template | null
  onOpen?: (id: string) => void
  previewOutcome?: PollResult | null
  liveStatus?: string | null
}) {
  const baseUrl = result.app_url || result.url || ''
  const minted = usePreviewLink(baseUrl ? result.id : undefined, result.cdp_token_port ?? null)
  const url = baseUrl && result.cdp_token
    ? `${baseUrl}${baseUrl.includes('?') ? '&' : '?'}token=${encodeURIComponent(result.cdp_token)}`
    : minted || baseUrl
  const [copiedUrl, setCopiedUrl] = useState(false)
  const [copiedId, setCopiedId] = useState(false)
  const urlTimer = useRef<number | undefined>(undefined)
  const idTimer = useRef<number | undefined>(undefined)
  useEffect(
    () => () => {
      window.clearTimeout(urlTimer.current)
      window.clearTimeout(idTimer.current)
    },
    [],
  )

  function copyUrl() {
    if (!url) return
    navigator.clipboard?.writeText(url).then(
      () => {
        setCopiedUrl(true)
        window.clearTimeout(urlTimer.current)
        urlTimer.current = window.setTimeout(() => setCopiedUrl(false), 2000)
      },
      () => {},
    )
  }

  function copyId() {
    navigator.clipboard?.writeText(result.id).then(
      () => {
        setCopiedId(true)
        window.clearTimeout(idTimer.current)
        idTimer.current = window.setTimeout(() => setCopiedId(false), 2000)
      },
      () => {},
    )
  }

  const verdict = previewOutcome?.state
  const dead = liveStatus === 'crashed' || liveStatus === 'exited'
  const isReady = !dead && (!url || verdict === undefined || verdict === 'ready')
  const isIndeterminate = !dead && verdict === 'unknown'

  // Glyph, tone and headline are one decision. Deriving all three from a single
  // outcome keeps them from drifting apart.
  const outcome = launchOutcome({ dead, liveStatus, isReady, isIndeterminate })

  const expiresAtSec = result.expires_at ? Math.floor(Date.parse(result.expires_at) / 1000) : 0
  const countdown = formatCountdown(expiresAtSec, Math.floor(Date.now() / 1000))
  let expiryText = ''
  if (countdown) expiryText = countdown === 'expired' ? 'Expired' : `Stops in ${countdown}`

  // One arm per shape of result: a service with a connection string, an app
  // with a preview URL, or a sandbox with neither.
  function readyBody() {
    const copyIdBtn = (
      <button type="button" className={`btn dp-copy${copiedId ? ' copied' : ''}`} onClick={copyId}>
        {copiedId ? 'Copied ✓' : 'Copy ID'}
      </button>
    )
    const openBtn = (primary: boolean) => onOpen && (
      <button type="button" className={primary ? 'btn primary' : 'btn'} onClick={() => onOpen(result.id)}>
        Open sandbox →
      </button>
    )

    if (service) {
      return (
        <>
          <ServiceConnBlock conn={serviceConn(service, result.id)} />
          <div className="dp-actions">{openBtn(true)}{copyIdBtn}</div>
        </>
      )
    }

    if (url) {
      return (
        <>
          <div className="dep-preview dp-preview">
            <span className="dep-preview-lbl">Preview URL</span>
            {url}
          </div>
          <div className="dp-actions">
            <a className="btn primary" href={url} target="_blank" rel="noopener noreferrer">
              Open in new tab ↗
            </a>
            {openBtn(false)}
            <button type="button" className={`btn dp-copy${copiedUrl ? ' copied' : ''}`} onClick={copyUrl}>
              {copiedUrl ? 'Copied ✓' : 'Copy URL'}
            </button>
            {copyIdBtn}
          </div>
          <p className="lm-hint">
            Opens your app — it auto-refreshes while the sandbox boots (a few seconds).
          </p>
        </>
      )
    }

    return (
      <>
        <div className="dp-actions">{openBtn(true)}{copyIdBtn}</div>
        <p className="lm-hint">
          This template has no built-in web app — open the sandbox to use its terminal, upload
          files, or build an app with AI.
        </p>
      </>
    )
  }

  return (
    <div className="dp-card dp-ready">
      <div className="dp-ready-head">
        <span className="dp-check" aria-hidden="true">{outcome.glyph}</span>
        <div>
          <div className={`dp-title ${outcome.tone}`}>{outcome.title}</div>
          <div className="dp-id mono">{result.id}</div>
          {dead && (
            <div className="dp-expiry">
              The sandbox page’s Logs tab has the exit — fix the start command and deploy again.
            </div>
          )}
          {!dead && expiryText && <div className="dp-expiry">{expiryText}</div>}
        </div>
      </div>
      {!isReady && !isIndeterminate && previewOutcome && (
        <PreviewDiagnosis
          outcome={previewOutcome}
          sandboxId={result.id}
          onOpen={onOpen}
          deployLog={result.deploy_log}
        />
      )}

      {readyBody()}

      {result.deploy_log && (
        <details className="dp-log">
          <summary>Deploy log</summary>
          <div className="deploy-log">{result.deploy_log}</div>
        </details>
      )}

      <div className="dp-actions dp-actions-foot">
        <button type="button" className="btn" onClick={onReset}>
          Deploy another
        </button>
      </div>
    </div>
  )
}

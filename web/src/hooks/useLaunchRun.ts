import { useCallback, useEffect, useRef, useState } from 'react'
import {
  createSandbox, destroySandbox, getPreviewStatus, getSandbox, postLaunchSample,
} from '../api'
import type { CreateSandboxReq, CreateSandboxResp, Template } from '../types'
import type { Kind } from '../lib/launchKinds'
import { cancellableCreate } from '../lib/cancellableCreate'
import { pollPreviewReady, type PollResult } from '../lib/previewStatus'
import { probeSettles, REVERIFY_INTERVAL_MS, REVERIFY_MAX_TICKS } from '../lib/reverify'
import { track } from '../lib/telemetry'

export type LaunchPhase = 'form' | 'running' | 'done' | 'error'

/**
 * useLaunchRun owns everything that happens after the deploy form is submitted:
 * the cancellable create, the readiness probe, and the two post-launch
 * watchers (preview re-verify and sandbox status).
 *
 * `onLaunched` fires once a create succeeds, so the page can record the launch
 * in its recents list.
 */
export function useLaunchRun(
  kind: Kind | undefined,
  onLaunched?: (template: Template, body: CreateSandboxReq) => void,
) {
  const [phase, setPhase] = useState<LaunchPhase>('form')
  const [result, setResult] = useState<CreateSandboxResp | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [verifying, setVerifying] = useState(false)
  const [previewOutcome, setPreviewOutcome] = useState<PollResult | null>(null)
  const [lastAttempt, setLastAttempt] = useState<{ body: CreateSandboxReq; template: Template } | null>(null)
  const [liveStatus, setLiveStatus] = useState<string | null>(null)

  const cancelRef = useRef<(() => void) | null>(null)
  const onLaunchedRef = useRef(onLaunched)
  onLaunchedRef.current = onLaunched

  const ready = previewOutcome?.state === 'ready'

  // A preview that did not come up on the first probe is retried on a timer;
  // a slow start should resolve itself without the user reloading.
  useEffect(() => {
    if (phase !== 'done' || !result || !previewOutcome || ready) return
    const id = result.id
    const port = lastAttempt?.body.port
    let ticks = 0
    const timer = window.setInterval(() => {
      if (++ticks > REVERIFY_MAX_TICKS) {
        window.clearInterval(timer)
        return
      }
      getPreviewStatus(id, port)
        .then(s => {
          if (!probeSettles(s.state)) return
          setPreviewOutcome({ state: s.state, lastState: s.state, upstreamStatus: s.upstream_status })
          if (s.state === 'ready') window.clearInterval(timer)
        })
        .catch(() => {})
    }, REVERIFY_INTERVAL_MS)
    return () => window.clearInterval(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase, result?.id, ready])

  // Separate from the preview watcher: a sandbox can crash with a preview that
  // never became ready, and the two states answer different questions.
  useEffect(() => {
    if (phase !== 'done' || !result) return
    setLiveStatus(null)
    const id = result.id
    let ticks = 0
    const timer = window.setInterval(() => {
      if (++ticks > REVERIFY_MAX_TICKS) {
        window.clearInterval(timer)
        return
      }
      getSandbox(id)
        .then(d => {
          setLiveStatus(d.status ?? null)
          if (d.status === 'crashed' || d.status === 'exited') window.clearInterval(timer)
        })
        .catch(() => {})
    }, REVERIFY_INTERVAL_MS)
    return () => window.clearInterval(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase, result?.id])

  const run = useCallback(async (body: CreateSandboxReq, t: Template, defaultPort?: number) => {
    if (phase === 'running') return
    setPhase('running')
    setError(null)
    setResult(null)
    setPreviewOutcome(null)
    setLastAttempt({ body, template: t })

    try {
      const launchT0 = Date.now()
      const handle = cancellableCreate({ create: () => createSandbox(body), destroy: destroySandbox })
      cancelRef.current = handle.cancel
      const created = await handle.result
      cancelRef.current = null
      if (created.cancelled) {
        setPhase('form')
        track('launch_outcome', { template: t.id, state: 'cancelled' })
        return
      }

      const resp = created.resp
      setResult(resp)
      onLaunchedRef.current?.(t, body)

      const target = kind === 'service' ? '' : resp.viewer_url || resp.app_url || resp.url
      if (target) {
        setVerifying(true)
        const port = body.port ?? defaultPort
        const sleep = (ms: number) => new Promise<void>(r => { window.setTimeout(r, ms) })
        const outcome = await pollPreviewReady({
          probe: () => getPreviewStatus(resp.id, port),
          sleep, now: () => Date.now(), deadlineMs: 60_000,
        })
        setPreviewOutcome(outcome)
        if (outcome.state === 'ready') {
          void postLaunchSample(t.id, Date.now() - launchT0).catch(() => {})
        }
        track('launch_outcome', {
          template: t.id, state: outcome.state, upstream_status: outcome.upstreamStatus,
        })
        setVerifying(false)
      } else {
        setPreviewOutcome(null)
      }
      setPhase('done')
    } catch (err) {
      setError((err as Error).message)
      setPhase('error')
      track('launch_outcome', { template: t.id, state: 'create_failed' })
    }
  }, [phase, kind])

  const cancel = useCallback(() => { cancelRef.current?.() }, [])

  const reset = useCallback(() => {
    setPhase('form')
    setResult(null)
    setError(null)
    setPreviewOutcome(null)
    setVerifying(false)
  }, [])

  return {
    phase, result, error, verifying, previewOutcome, lastAttempt, liveStatus,
    run, cancel, reset, setPhase,
  }
}

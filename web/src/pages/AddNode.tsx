import { useEffect, useRef, useState } from 'react'
import { mintJoinToken, listNodes } from '../api'
import type { JoinToken, NodeInfo } from '../types'

function generateLabel(busy: boolean, hasResult: boolean): string {
  if (busy) return 'Generating…'
  return hasResult ? 'Generate another' : 'Generate join command'
}

export default function AddNode() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<JoinToken | null>(null)
  const [copied, setCopied] = useState(false)
  const [copiedCloud, setCopiedCloud] = useState(false)

  const [watching, setWatching] = useState(false)
  const [joined, setJoined] = useState<NodeInfo | null>(null)
  const baselineIds = useRef<Set<string>>(new Set())

  async function generate() {
    setBusy(true)
    setError(null)
    setResult(null)
    setCopied(false)
    setJoined(null)
    setWatching(false)
    try {
      try {
        const { nodes } = await listNodes()
        baselineIds.current = new Set(nodes.map(n => n.id))
      } catch {
        baselineIds.current = new Set()
      }
      setResult(await mintJoinToken())
      setWatching(true)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  useEffect(() => {
    if (!watching) return
    const started = Date.now()
    const timer = setInterval(async () => {
      if (Date.now() - started > 10 * 60 * 1000) {
        setWatching(false)
        return
      }
      try {
        const { nodes } = await listNodes()
        const fresh = nodes.find(n => !baselineIds.current.has(n.id))
        if (fresh) {
          setJoined(fresh)
          setWatching(false)
        }
      } catch {
      }
    }, 3000)
    return () => clearInterval(timer)
  }, [watching])

  function copyCommand() {
    if (!result?.bootstrap_cmd) return
    navigator.clipboard.writeText(result.bootstrap_cmd).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    })
  }

  const cloudInit = result ? `#cloud-config\nruncmd:\n  - ${result.bootstrap_cmd}\n` : ''

  function copyCloudInit() {
    if (!cloudInit) return
    navigator.clipboard.writeText(cloudInit).then(() => {
      setCopiedCloud(true)
      setTimeout(() => setCopiedCloud(false), 2000)
    })
  }

  const minutesLeft = result
    ? Math.max(0, Math.round((result.expires_at * 1000 - Date.now()) / 60000))
    : 0

  function joinStatus() {
    if (joined) {
      return (
        <div className="callout success" style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ fontSize: 16 }}>✓</span>
          <span>
            <strong>{joined.id}</strong> joined the fleet — {joined.status}
            {joined.healthy ? ' (healthy)' : ''}. See it under <strong>Fleet</strong>.
          </span>
        </div>
      )
    }
    if (watching) {
      return (
        <div className="callout" style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span className="spinner" aria-hidden />
          <span>Waiting for the node to join… run the command on your host.</span>
        </div>
      )
    }
    return (
      <div className="est-hint">
        Not watching anymore. Click <strong>Generate another</strong> to watch again, or
        check the <strong>Fleet</strong> page for status.
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Add Node</h1>
          <p>
            Generate a one-time join command, then paste it on the new host (SSH or
            the provider console). It prefers full microVM isolation on KVM hosts
            and falls back to Docker, joins the fleet with an auto-assigned id, and
            appears below within a few seconds. No NODE_ID to pick and no secrets in
            the command.
          </p>
        </div>
      </div>

      <div style={{ maxWidth: 760 }}>
        {error && (
          <div className="callout error" style={{ marginBottom: 18 }}>
            <strong>Could not generate a join command</strong>
            <pre>{error}</pre>
          </div>
        )}

        <button className="btn primary" disabled={busy} onClick={generate}>
          {generateLabel(busy, !!result)}
        </button>

        {result && (
          <div style={{ marginTop: 22 }}>
            <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>
              Run this on the new host
            </div>
            <div className="callout success">
              <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12 }}>
                <div className="deploy-log" style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}>
                  {result.bootstrap_cmd}
                </div>
                <button className="copy-btn" onClick={copyCommand}>{copied ? '✓ Copied' : 'Copy'}</button>
              </div>
            </div>

            <div style={{ marginTop: 18 }}>
              <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>
                Or add this to cloud-init / user-data (self-onboards on first boot — no login)
              </div>
              <div className="callout">
                <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12 }}>
                  <div className="deploy-log" style={{ flex: 1, wordBreak: 'break-all', whiteSpace: 'pre-wrap' }}>
                    {cloudInit}
                  </div>
                  <button className="copy-btn" onClick={copyCloudInit}>{copiedCloud ? '✓ Copied' : 'Copy'}</button>
                </div>
              </div>
              <div className="est-hint" style={{ marginTop: 8 }}>
                Paste into the instance's <strong>cloud-init / user data</strong> field when you
                create it. The box runs it on first boot and joins automatically.
              </div>
            </div>

            <div style={{ marginTop: 16 }}>
              {joinStatus()}
            </div>

            <div className="est-hint" style={{ marginTop: 16 }}>
              One-time token — valid for ~{minutesLeft} min. Run it soon; generate a
              fresh one if it expires. For zero-touch onboarding, paste the same
              command into the instance's cloud-init / user-data at creation.
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

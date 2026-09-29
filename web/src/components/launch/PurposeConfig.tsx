import { useState } from 'react'

import type { Template } from '../../types'
import { putSecret } from '../../api'
import { ModeLadder } from '../agent/ModeLadder'

import { canPick } from '../../lib/mcpCatalog'
import McpPicker from '../McpPicker'

import { type AccessMode, ACCESS_MODES, envPlaceholder } from '../../lib/launchKinds'

// Verdict templates ship one CLI each; anything else has no runner to name.
const VERDICT_CLI: Record<string, string> = { loadtest: 'pdload', security: 'pdscan' }

import { useLaunchConfig } from './useLaunchConfig'

export function StartConfig({
  tpl,
  start,
  onStartChange,
  port,
  onPortChange,
  previewPort,
}: {
  tpl: Template
  start: string
  onStartChange: (v: string) => void
  port: string
  onPortChange: (v: string) => void
  previewPort: number
}) {
  if (tpl.has_server) {
    return (
      <div className="lm-note">
        <b>Built-in server.</b>{' '}
        {tpl.server_desc || 'This template serves a built-in app on the preview URL.'} Preview
        port: <code>{tpl.default_port ?? tpl.port ?? previewPort}</code>. No start command needed.
      </div>
    )
  }

  return (
    <>
      <div className="form-group lm-fg">
        <label>
          Start command{' '}
          <span className="lm-opt-note">optional — leave blank for just a shell</span>
        </label>
        <input
          value={start}
          onChange={e => onStartChange(e.target.value)}
          placeholder={tpl.start_suggestion || 'e.g. python3 -m http.server $PORT'}
        />
        <p className="lm-help">
          Use <code>$PORT</code> so your app binds the port the preview serves.
        </p>
      </div>
      <div className="form-group lm-fg lm-port">
        <label>Port</label>
        <input
          value={port}
          onChange={e => onPortChange(e.target.value)}
          inputMode="numeric"
          placeholder="8000"
        />
        <p className="lm-help">
          Your app must bind <code>$PORT</code>; the preview serves this port.
        </p>
      </div>
    </>
  )
}

export function PurposeConfig({
  cfg,
}: {
  cfg: ReturnType<typeof useLaunchConfig>
}) {
  const { tpl, kind } = cfg
  if (!tpl) return null

  if (kind === 'agent' && cfg.agent) {
    return <AgentPurposeConfig cfg={cfg} />
  }

  return <NonAgentPurposeConfig cfg={cfg} />
}

function AgentPurposeConfig({ cfg }: { cfg: ReturnType<typeof useLaunchConfig> }) {
  const { tpl } = cfg
  const [manualKey, setManualKey] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveErr, setSaveErr] = useState<string | null>(null)
  if (!tpl || !cfg.agent) return null
  {
    const a = cfg.agent
    const mode = ACCESS_MODES[cfg.accessMode]
    const vaultConnected = !!a.keyEnv && cfg.vaultNames.includes(a.keyEnv) && !manualKey

    function keySection() {
      if (a.keyEnv && vaultConnected) {
        return (
          <>
            <div className="lm-sec-h">
              {a.keyLabel} <span className="lm-opt-note">from your vault</span>
            </div>
            <p className="lm-help">
              ✓ Connected — injected as <code>${a.keyEnv}</code> at launch. The key never leaves
              the server.{' '}
              <button type="button" className="lm-linkbtn" onClick={() => setManualKey(true)}>
                Use a different key
              </button>
            </p>
          </>
        )
      }
      if (a.keyEnv) {
        return (
          <>
            <div className="lm-sec-h">
              {a.keyLabel}
              <span className="lm-req"> *</span>{' '}
              <span className="lm-opt-note">stored as ${a.keyEnv}, required</span>
            </div>
            <input
              className="pa-input mono"
              type="password"
              value={cfg.apiKey}
              autoComplete="off"
              placeholder="sk-…"
              onChange={e => cfg.setApiKey(e.target.value)}
            />
            {cfg.apiKey.trim() ? (
              <p className="lm-help">
                Used for this launch.{' '}
                <button
                  type="button"
                  className="lm-linkbtn"
                  disabled={saving}
                  onClick={saveKeyToVault}
                >
                  {saving ? 'Saving…' : 'Save to vault for next time'}
                </button>{' '}
                — then every {tpl?.name || tpl?.id} launch is one click.
              </p>
            ) : (
              <p className="lm-hint pa-warn">
                No key yet — deploying now just opens an idle terminal. Add a key to actually run
                tasks.
              </p>
            )}
            {saveErr && <p className="lm-error">{saveErr}</p>}
            {manualKey && (
              <p className="lm-help">
                <button
                  type="button"
                  className="lm-linkbtn"
                  onClick={() => {
                    setManualKey(false)
                    cfg.setApiKey('')
                  }}
                >
                  ← Back to the vault key
                </button>
              </p>
            )}
          </>
        )
      }
      return (
        <>
          <div className="lm-sec-h">
            {a.keyLabel} <span className="lm-opt-note">provider-agnostic</span>
          </div>
          <p className="lm-help">{a.keyNote}</p>
        </>
      )
    }

    function saveKeyToVault() {
      const key = cfg.apiKey.trim()
      if (!a.keyEnv || !key || saving) return
      setSaving(true)
      setSaveErr(null)
      putSecret(a.keyEnv, key)
        .then(() => {
          cfg.refreshVault()
          cfg.setApiKey('')
          setManualKey(false)
        })
        .catch(err => setSaveErr((err as Error).message))
        .finally(() => setSaving(false))
    }
    return (
      <>
        <div className="lm-sec">
          <div className="lm-sec-h">
            Access{' '}
            <span className="lm-opt-note">how you reach the running agent → its preview URL</span>
          </div>
          <div className="seg pa-seg" role="tablist" aria-label="Access mode">
            {(['terminal', 'task'] as AccessMode[]).map(m => (
              <button
                key={m}
                type="button"
                role="tab"
                aria-selected={cfg.accessMode === m}
                className={`seg-btn${cfg.accessMode === m ? ' on' : ''}`}
                onClick={() => cfg.setAccessMode(m)}
              >
                {ACCESS_MODES[m].label}
              </button>
            ))}
          </div>
          <p className="lm-help">{mode.help}</p>
        </div>

        <div className="lm-sec">
          <div className="lm-sec-h">
            Autonomy{' '}
            <span className="lm-opt-note">how much it does without asking you</span>
          </div>
          <ModeLadder
            agent={cfg.tpl?.agent}
            value={cfg.agentMode}
            onChange={cfg.setAgentMode}
          />
        </div>

        <div className="lm-sec">
          {keySection()}

          {a.baseUrlEnv && (
            <div className="pa-subfield">
              <div className="lm-sec-h">
                API base URL / gateway <span className="lm-opt-note">optional</span>
              </div>
              <input
                className="pa-input mono"
                type="text"
                value={cfg.baseUrl}
                autoComplete="off"
                placeholder="https://gateway.example.com"
                onChange={e => cfg.setBaseUrl(e.target.value)}
              />
              <p className="lm-help">
                Point the agent at a gateway or custom endpoint (sets <code>${a.baseUrlEnv}</code>).
              </p>
            </div>
          )}

          {a.modelEnv && (
            <div className="pa-subfield">
              <div className="lm-sec-h">
                Model <span className="lm-opt-note">optional</span>
              </div>
              <input
                className="pa-input mono"
                type="text"
                value={cfg.model}
                autoComplete="off"
                placeholder="e.g. claude-sonnet-4-5"
                onChange={e => cfg.setModel(e.target.value)}
              />
              <p className="lm-help">
                Override the model (sets <code>${a.modelEnv}</code>).
              </p>
            </div>
          )}
        </div>

        <div className="lm-sec">
          <div className="lm-sec-h">
            {a.configLabel}{' '}
            <span className="lm-opt-note">
              written to <code>{a.mcpFile}</code> · {a.configFormat.toUpperCase()}
            </span>
          </div>
          {canPick(tpl.id) ? (
            <McpPicker
              agentId={tpl.id}
              format={a.configFormat}
              value={cfg.mcpJson}
              onChange={cfg.setMcpJson}
              vaultNames={cfg.vaultNames}
            />
          ) : (
            <textarea
              className="pa-textarea mono"
              rows={a.configFormat === 'toml' ? 7 : 9}
              spellCheck={false}
              value={cfg.mcpJson}
              onChange={e => cfg.setMcpJson(e.target.value)}
            />
          )}
          <p className="lm-help">
            <a href={a.docsUrl} target="_blank" rel="noopener noreferrer">Docs ↗</a>
          </p>
        </div>

        {mode.showTask && (
          <div className="lm-sec">
            <div className="lm-sec-h">
              Task <span className="lm-opt-note">optional — what should the agent do?</span>
            </div>
            <textarea
              className="pa-textarea"
              rows={3}
              value={cfg.task}
              placeholder={a.taskPlaceholder}
              onChange={e => cfg.setTask(e.target.value)}
            />
            <p className="lm-help">
              Runs <code>{a.runCmd.replace('{task}', '…')}</code>. Leave blank to open an
              interactive session.
            </p>
          </div>
        )}
      </>
    )
  }
}

function NonAgentPurposeConfig({ cfg }: { cfg: ReturnType<typeof useLaunchConfig> }) {
  const { tpl, kind } = cfg
  if (!tpl) return null

  if (kind === 'service') {
    const svcPort = tpl.default_port ?? tpl.port ?? cfg.previewPort
    const declared = tpl.env ?? []
    return (
      <>
        <div className="lm-sec">
          <div className="lm-sec-h">
            Connection{' '}
            <span className="lm-opt-note">reached at the sandbox host on port {svcPort}</span>
          </div>
          <div className="lm-note">
            This service listens on <b>port {svcPort}</b>. No start command — set the credentials
            below and connect via the sandbox host.
          </div>
        </div>
        {declared.length > 0 && (
          <div className="lm-sec">
            <div className="lm-sec-h">Credentials</div>
            <div className="lm-env">
              {declared.map(e => (
                <label key={e.name} className="lm-env-row">
                  <span className="lm-env-name">
                    {e.name}
                    {e.required && (
                      <span className="lm-req" title="required">
                        {' '}
                        *
                      </span>
                    )}
                  </span>
                  <input
                    type={e.secret ? 'password' : 'text'}
                    value={cfg.declaredEnv[e.name] ?? ''}
                    placeholder={envPlaceholder(e)}
                    autoComplete="off"
                    onChange={ev => cfg.setDeclaredEnvVar(e.name, ev.target.value)}
                  />
                </label>
              ))}
            </div>
          </div>
        )}
      </>
    )
  }

  if (kind === 'gui') {
    return (
      <div className="lm-sec">
        <div className="lm-note">
          <b>Built-in screen.</b>{' '}
          {tpl.server_desc || 'This template streams a graphical desktop on the preview URL.'}{' '}
          Preview port: <code>{tpl.default_port ?? tpl.port ?? cfg.previewPort}</code>. No start
          command — open the preview URL to see and control it.
        </div>
      </div>
    )
  }

  if (kind === 'test') {
    return (
      <>
        <div className="lm-sec">
          <div className="lm-sec-h">
            Test command <span className="lm-opt-note">runs your E2E suite</span>
          </div>
          <input
            className="pa-input mono"
            value={cfg.testCmd}
            onChange={e => cfg.setTestCmd(e.target.value)}
            placeholder="npx playwright test"
          />
          <p className="lm-help">
            Attach your test project (Advanced → Files), then set the command — on finish the HTML
            report is served on the preview URL. Leave blank to just open a terminal.
          </p>
        </div>
        <div className="lm-sec">
          <button
            type="button"
            className="pa-toggle"
            aria-pressed={cfg.serveReport}
            onClick={() => cfg.setServeReport(!cfg.serveReport)}
          >
            <span className={`pa-tg${cfg.serveReport ? ' on' : ''}`}>
              <i />
            </span>
            Serve the HTML report on preview
          </button>
        </div>
      </>
    )
  }

  if (kind === 'shell') {
    return (
      <div className="lm-sec">
        <div className="lm-sec-h">
          Start command{' '}
          <span className="lm-opt-note">optional — leave blank for just a shell</span>
        </div>
        <input
          className="pa-input mono"
          value={cfg.start}
          onChange={e => cfg.setStart(e.target.value)}
          placeholder={tpl.start_suggestion || 'e.g. python3 -m http.server $PORT'}
        />
        <div className="lm-note" style={{ marginTop: 10 }}>
          This is <b>just a shell</b> — a minimal environment. Add a start command to serve
          something on the preview URL, or leave it blank and open an interactive session.
        </div>
      </div>
    )
  }

  if (tpl.kind === 'verdict' && tpl.primary_input) {
    const pi = tpl.primary_input
    const runner = VERDICT_CLI[tpl.id] ?? null
    if (runner) {
      return (
        <div className="lm-sec">
          <div className="lm-sec-h">
            {pi.label}
            {pi.required && <span className="lm-req"> *</span>}
          </div>
          <input
            className="pa-input mono"
            type="url"
            value={cfg.primaryValue}
            placeholder="https://…"
            onChange={e => cfg.setPrimaryValue(e.target.value)}
          />
          {cfg.primaryValue.trim() ? (
            <p className="lm-help">
              <code>{runner}</code> runs against it on boot and the GREEN/RED report replaces the
              preview when it finishes. Only test targets you own.
            </p>
          ) : (
            <p className="lm-hint pa-warn">
              Required — without a target the sandbox has nothing to {tpl.id === 'loadtest' ? 'load-test' : 'scan'} and
              the preview shows an empty “No run yet” page.
            </p>
          )}
        </div>
      )
    }
  }


  return (
    <StartConfig
      tpl={tpl}
      start={cfg.start}
      onStartChange={cfg.setStart}
      port={cfg.port}
      onPortChange={cfg.setPort}
      previewPort={cfg.previewPort}
    />
  )
}

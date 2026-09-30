import { useState, useEffect, useRef } from 'react'

import type { Template, TemplateEnv, Plan } from '../../types'

import { TemplateIcon } from '../templateIcons'

import { LIFETIME_CHOICES, type LifetimeKey } from '../../lib/lifetime'
import { serialisePilldropsConfig, PILLDROPS_FILE, type PilldropsConfig } from '../../lib/pilldropsConfig'

import { type AccessMode, type CustomEnv, type Kind, type ServiceConn, ACCESS_MODES, ctaLabel, envPlaceholder, previewLabel, runsOnLabel, serviceConn, sizeLabel, withCur } from '../../lib/launchKinds'

export function EnvEditor({
  declared,
  values,
  onDeclaredChange,
  custom,
  onCustomChange,
}: {
  declared: TemplateEnv[]
  values: Record<string, string>
  onDeclaredChange: (name: string, value: string) => void
  custom: CustomEnv[]
  onCustomChange: (rows: CustomEnv[]) => void
}) {
  return (
    <div className="lm-env">
      {declared.map(e => (
        <label key={e.name} className="lm-env-row">
          <span className="lm-env-name">
            {e.name}
            {e.required && <span className="lm-req" title="required"> *</span>}
            {e.secret && <span className="lm-secret" title="stored as a secret"> secret</span>}
          </span>
          <input
            type={e.secret ? 'password' : 'text'}
            value={values[e.name] ?? ''}
            placeholder={envPlaceholder(e)}
            autoComplete="off"
            onChange={ev => onDeclaredChange(e.name, ev.target.value)}
          />
        </label>
      ))}

      {custom.map((row, i) => (
        <div key={i} className="lm-env-custom">
          <input
            className="lm-env-k"
            value={row.name}
            placeholder="NAME"
            autoComplete="off"
            onChange={ev => {
              const next = custom.slice()
              next[i] = { ...next[i], name: ev.target.value }
              onCustomChange(next)
            }}
          />
          <span className="lm-eq">=</span>
          <input
            className="lm-env-v"
            value={row.value}
            placeholder="VALUE"
            autoComplete="off"
            onChange={ev => {
              const next = custom.slice()
              next[i] = { ...next[i], value: ev.target.value }
              onCustomChange(next)
            }}
          />
          <button
            type="button"
            className="iconbtn"
            aria-label="Remove variable"
            title="Remove"
            onClick={() => onCustomChange(custom.filter((_, j) => j !== i))}
          >
            ×
          </button>
        </div>
      ))}

      <button
        type="button"
        className="btn ghost lm-add-env"
        onClick={() => onCustomChange([...custom, { name: '', value: '' }])}
      >
        ＋ Add variable
      </button>
    </div>
  )
}

export function CopyCli({ snippet }: { snippet: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="lm-cli">
      <pre className="lm-cli-code">{snippet}</pre>
      <button
        type="button"
        className="copy-btn lm-cli-copy"
        onClick={() => {
          navigator.clipboard?.writeText(snippet).then(
            () => {
              setCopied(true)
              setTimeout(() => setCopied(false), 1400)
            },
            () => {},
          )
        }}
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

export function ExportPilldropsConfig({ config }: { config: PilldropsConfig }) {
  const [copied, setCopied] = useState(false)

  function download() {
    const text = serialisePilldropsConfig(config)
    const blob = new Blob([text], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = PILLDROPS_FILE
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
  }

  function copy() {
    navigator.clipboard?.writeText(serialisePilldropsConfig(config)).then(
      () => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1400)
      },
      () => {},
    )
  }

  return (
    <div className="dep-export-cfg">
      <button
        type="button"
        className="btn ghost"
        title={`Download the current configuration as ${PILLDROPS_FILE}, for committing to your repo`}
        onClick={download}
      >
        ⬇ Save as {PILLDROPS_FILE}
      </button>
      <button type="button" className="btn ghost" title="Copy to clipboard" onClick={copy}>
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

export function TemplateHeader({ tpl }: { tpl: Template }) {
  const langBadge = tpl.language || tpl.tags?.[0]
  return (
    <div className="lm-head">
      <span className="lm-head-icon"><TemplateIcon t={tpl} /></span>
      <div className="lm-head-txt">
        <div className="lm-head-name">
          {tpl.name || tpl.id}
          {langBadge && <span className="lm-lang">{langBadge}</span>}
        </div>
        <p className="lm-head-desc">{tpl.description}</p>
        {tpl.docs_url && (
          <a className="lm-docs" href={tpl.docs_url} target="_blank" rel="noopener noreferrer">
            Docs ↗
          </a>
        )}
      </div>
    </div>
  )
}

export function WhatsInside({ tpl }: { tpl: Template }) {
  if (!tpl.preinstalled || tpl.preinstalled.length === 0) return null
  return (
    <div className="lm-sec">
      <div className="lm-sec-h">What’s inside</div>
      <div className="lm-chips">
        {tpl.preinstalled.map(p => (
          <span key={p} className="chip">
            {p}
          </span>
        ))}
      </div>
    </div>
  )
}

export function DefaultsSummary({
  tpl,
  cpu,
  mem,
  storage,
  previewPort,
}: {
  tpl: Template
  cpu: number
  mem: number
  storage: number
  previewPort: number
}) {
  return (
    <div className="lm-defaults">
      <span className="lm-defaults-label">Defaults</span>
      <span>
        {cpu} vCPU · {sizeLabel(mem)} RAM · {sizeLabel(storage)} storage
      </span>
      <span className="lm-dot">·</span>
      <span className="lm-defaults-label">Runs on</span>
      <span>{runsOnLabel(tpl)}</span>
      <span className="lm-dot">·</span>
      <span>preview port {previewPort}</span>
    </div>
  )
}

export function ResourcePicker({
  cpu,
  mem,
  storage,
  onCpu,
  onMem,
  onStorage,
  plan,
}: {
  cpu: number
  mem: number
  storage: number
  onCpu: (n: number) => void
  onMem: (n: number) => void
  onStorage: (n: number) => void
  plan?: Plan | null
}) {
  const cpuCap = plan?.max_vcpu || Infinity
  const memCap = plan?.max_mem_mb || Infinity
  const storageCap = plan?.max_storage_mb || Infinity
  const overCap = cpu > cpuCap || mem > memCap || storage > storageCap
  return (
    <>
      <div className="lm-res">
        <label className="lm-res-item">
          CPU
          <select value={cpu} onChange={e => onCpu(parseInt(e.target.value, 10))}>
            {withCur([1, 2, 4, 8], cpu).map(n => (
              <option key={n} value={n} disabled={n > cpuCap && n !== cpu}>
                {n} vCPU{n > cpuCap ? ' — above plan' : ''}
              </option>
            ))}
          </select>
        </label>
        <label className="lm-res-item">
          RAM
          <select value={mem} onChange={e => onMem(parseInt(e.target.value, 10))}>
            {withCur([512, 1024, 2048, 4096, 8192], mem).map(n => (
              <option key={n} value={n} disabled={n > memCap && n !== mem}>
                {sizeLabel(n)}{n > memCap ? ' — above plan' : ''}
              </option>
            ))}
          </select>
        </label>
        <label className="lm-res-item">
          Storage
          <select value={storage} onChange={e => onStorage(parseInt(e.target.value, 10))}>
            {withCur([1024, 2048, 4096, 8192, 20480], storage).map(n => (
              <option key={n} value={n} disabled={n > storageCap && n !== storage}>
                {sizeLabel(n)}{n > storageCap ? ' — above plan' : ''}
              </option>
            ))}
          </select>
        </label>
      </div>
      {overCap && plan && (
        <p className="lm-hint pa-warn">
          Your {plan.name || plan.id} plan caps sandboxes at {plan.max_vcpu} vCPU /{' '}
          {sizeLabel(plan.max_mem_mb)} / {sizeLabel(plan.max_storage_mb)} storage — larger requests
          are clamped at create.
        </p>
      )}
    </>
  )
}

export function LifetimePicker({
  value,
  onChange,
}: {
  value: LifetimeKey
  onChange: (v: LifetimeKey) => void
}) {
  const choice = LIFETIME_CHOICES.find(c => c.key === value) ?? LIFETIME_CHOICES[0]
  return (
    <div className="lm-fg">
      <label className="lm-adv-label">Lifetime</label>
      <select
        className="sel"
        value={value}
        onChange={e => onChange(e.target.value as LifetimeKey)}
        aria-label="Lifetime"
      >
        {LIFETIME_CHOICES.map(c => (
          <option key={c.key} value={c.key}>
            {c.label}
          </option>
        ))}
      </select>
      <span className="lm-opt-note">{choice.note}</span>
    </div>
  )
}

export function PanelHeader({ tpl, onBack }: { tpl: Template; onBack?: () => void }) {
  return (
    <div className="dep-ph">
      {onBack && (
        <button type="button" className="dep-ph-back" onClick={onBack} aria-label="Back to templates">
          ←
        </button>
      )}
      <span className="dep-ph-ic"><TemplateIcon t={tpl} /></span>
      <div className="dep-ph-txt">
        <div className="dep-ph-nm">{tpl.name || tpl.id}</div>
        <div className="dep-ph-desc">{tpl.description}</div>
      </div>
    </div>
  )
}

function copyConnLabel(copied: boolean, hasUrl: boolean): string {
  if (copied) return 'Copied ✓'
  return hasUrl ? 'Copy connection URL' : 'Copy host:port'
}

export function ServiceConnBlock({ conn }: { conn: ServiceConn }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const hostPort = `${conn.host}:${conn.port}`
  const copyText = conn.url ?? hostPort
  function copyHostPort() {
    navigator.clipboard?.writeText(copyText).then(
      () => {
        setCopied(true)
        window.clearTimeout(timer.current)
        timer.current = window.setTimeout(() => setCopied(false), 2000)
      },
      () => {},
    )
  }
  return (
    <div className="dep-conn">
      <div className="dep-conn-lbl">Connection</div>
      <div className="dep-conn-row">
        <span className="dep-conn-k">Host</span>
        <code className="dep-conn-v mono">{conn.host}</code>
      </div>
      <div className="dep-conn-row">
        <span className="dep-conn-k">Port</span>
        <code className="dep-conn-v mono">{conn.port}</code>
      </div>
      {conn.credEnvNames.length > 0 && (
        <div className="dep-conn-row">
          <span className="dep-conn-k">Credentials</span>
          <span className="dep-conn-creds">
            {conn.credEnvNames.map(n => (
              <code key={n} className="chip mono">
                {n}
              </code>
            ))}
          </span>
        </div>
      )}
      {conn.url && (
        <div className="dep-conn-row">
          <span className="dep-conn-k">URL</span>
          <code className="dep-conn-v mono" style={{ wordBreak: 'break-all' }}>{conn.url}</code>
        </div>
      )}
      <button
        type="button"
        className={`btn dp-copy${copied ? ' copied' : ''}`}
        onClick={copyHostPort}
      >
        {copyConnLabel(copied, !!conn.url)}
      </button>
      <p className="lm-hint">
        This is a database endpoint (not a web page) — connect with your client using the host,
        port and credentials above. Host is best-effort; use the sandbox host if it differs.
      </p>
    </div>
  )
}

export function SummaryPanel({
  tpl,
  kind,
  accessMode,
  cpu,
  mem,
  previewPort,
  previewUrl,
  cliSnippet,
  busy,
  error,
  onSubmit,
  ctaDisabledNote,
}: {
  tpl: Template
  kind: Kind
  accessMode: AccessMode
  cpu: number
  mem: number
  previewPort: number
  previewUrl: string
  cliSnippet: string
  busy: boolean
  error?: string | null
  onSubmit?: () => void
  ctaDisabledNote?: string
}) {
  const cta = ctaLabel(kind, accessMode)
  return (
    <aside className="dep-summary">
      <h4>Summary</h4>
      <div className="dep-kv">
        <span>Template</span>
        <b>{tpl.name || tpl.id}</b>
      </div>
      <div className="dep-kv">
        <span>Purpose</span>
        <b style={{ textTransform: 'capitalize' }}>{kind}</b>
      </div>
      {kind === 'agent' && (
        <div className="dep-kv">
          <span>Access</span>
          <b>{ACCESS_MODES[accessMode].access}</b>
        </div>
      )}
      <div className="dep-kv">
        <span>Runs on</span>
        <b>{runsOnLabel(tpl)}</b>
      </div>
      <div className="dep-kv">
        <span>Resources</span>
        <b>
          {cpu} vCPU · {sizeLabel(mem)}
        </b>
      </div>
      <div className="dep-kv">
        <span>{kind === 'service' ? 'Port' : 'Preview port'}</span>
        <b>{previewPort}</b>
      </div>

      {kind === 'service' ? (
        <ServiceConnBlock conn={serviceConn(tpl)} />
      ) : (
        <div className="dep-preview">
          <span className="dep-preview-lbl">{previewLabel(kind, accessMode)}</span>
          {previewUrl}
        </div>
      )}

      <div className="dep-cli">
        <div className="dep-cli-lbl">Copy as CLI</div>
        <CopyCli snippet={cliSnippet} />
      </div>

      {error && <p className="lm-error">{error}</p>}

      <button
        type={onSubmit ? 'button' : 'submit'}
        className="btn primary dep-deploy-btn"
        disabled={busy || !!ctaDisabledNote}
        onClick={onSubmit}
      >
        {busy ? 'Working…' : cta}
      </button>
      {ctaDisabledNote && !busy && <p className="lm-hint pa-warn">{ctaDisabledNote}</p>}
    </aside>
  )
}

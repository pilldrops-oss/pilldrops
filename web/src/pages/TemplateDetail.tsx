import { useEffect, useState } from 'react'
import { useParams, Link } from 'react-router-dom'
import { listTemplates } from '../api'
import type { Template } from '../types'
import {
  categoryOf,
  sizeLabel,
  runsOnLabel,
  WhatsInside,
} from '../components/launchConfig'
import { TemplateIcon } from '../components/templateIcons'
import { RowsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'

export default function TemplateDetail() {
  const { id } = useParams<{ id: string }>()
  const [templates, setTemplates] = useState<Template[]>([])
  const [state, setState] = useState<'loading' | 'ready' | 'error'>('loading')

  function load() {
    setState('loading')
    listTemplates()
      .then(ts => {
        setTemplates(ts ?? [])
        setState('ready')
      })
      .catch(() => setState('error'))
  }
  useEffect(load, [])

  if (state === 'loading') {
    return (
      <div className="wrap">
        <div className="panel">
          <RowsSkeleton rows={4} />
        </div>
      </div>
    )
  }

  if (state === 'error') {
    return (
      <div className="wrap">
        <ErrorState
          error={new Error('Something went wrong loading templates.')}
          onRetry={load}
          title="Couldn’t load templates"
        />
      </div>
    )
  }

  const tpl = templates.find(t => t.id === id)

  if (!tpl) {
    return (
      <div className="wrap">
        <ErrorState error={new Error('We couldn’t find that template.')} title="Not found" />
        <p style={{ marginTop: 14 }}>
          <Link to="/deploy">← Back to the catalog</Link>
        </p>
      </div>
    )
  }

  const cpu = tpl.cpu || 1
  const mem = tpl.memory_mb || 512
  const storage = tpl.storage_mb || 1024
  const port = tpl.default_port ?? tpl.port

  return (
    <div className="wrap">
      <p>
        <Link to="/deploy">← Back to the catalog</Link>
      </p>

      <div className="h1row">
        <div style={{ display: 'flex', alignItems: 'flex-start', gap: 14 }}>
          <span style={{ fontSize: 34, lineHeight: 1 }}>
            <TemplateIcon t={tpl} />
          </span>
          <div>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap' }}>
              <h1 className="h1" style={{ margin: 0 }}>
                {tpl.name || tpl.id}
              </h1>
              <span className="chip">{categoryOf(tpl)}</span>
              {tpl.language && <span className="chip">{tpl.language}</span>}
            </div>
            {tpl.description && (
              <p style={{ marginTop: 10, color: 'var(--sub)', maxWidth: 640 }}>{tpl.description}</p>
            )}
          </div>
        </div>
      </div>

      <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', margin: '18px 0 26px' }}>
        <Link to="/deploy" state={{ templateId: tpl.id }} className="btn primary">
          Launch
        </Link>
        <Link to="/deploy" state={{ templateId: tpl.id }} className="btn">
          Configure
        </Link>
        {tpl.docs_url && (
          <a className="btn ghost" href={tpl.docs_url} target="_blank" rel="noopener noreferrer">
            Docs ↗
          </a>
        )}
      </div>

      <div className="panel" style={{ padding: 20, display: 'grid', gap: 22, maxWidth: 720 }}>
        <WhatsInside tpl={tpl} />

        <div className="lm-sec">
          <div className="lm-sec-h">Serves</div>
          {tpl.has_server ? (
            <p className="lm-help">
              {tpl.server_desc || 'This template serves a built-in app on the preview URL.'}{' '}
              Preview port: <code>{port ?? 8000}</code>.
            </p>
          ) : (
            <p className="lm-help">
              No built-in web app — the terminal is where this template is used.
            </p>
          )}
        </div>

        {tpl.setup_suggestion && tpl.setup_suggestion.length > 0 && (
          <div className="lm-sec">
            <div className="lm-sec-h">Setup</div>
            <pre className="lm-cli-code">{tpl.setup_suggestion.join('\n')}</pre>
          </div>
        )}

        <div className="lm-sec">
          <div className="lm-sec-h">Resources</div>
          <p className="lm-help">
            {cpu} vCPU · {sizeLabel(mem)} RAM · {sizeLabel(storage)} storage · runs
            on {runsOnLabel(tpl)}
          </p>
        </div>

        <div className="lm-sec">
          <div className="lm-sec-h">CLI</div>
          <pre className="lm-cli-code">
            <code>{`pilldrops create --template ${tpl.id}`}</code>
          </pre>
        </div>
      </div>
    </div>
  )
}

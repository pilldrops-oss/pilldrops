import { useEffect, useState } from 'react'
import { TemplateIcon } from '../components/templateIcons'
import { useNavigate, Link } from 'react-router-dom'
import { listTemplates } from '../api'
import type { Template } from '../types'
import { RowsSkeleton } from '../components/Skeleton'
import ErrorState from '../components/ErrorState'

const ORANGE = '#FF8800'

function TemplateCard({ tpl }: { tpl: Template }) {
  const navigate = useNavigate()

  function use() {
    navigate('/deploy', { state: { templateId: tpl.id } })
  }

  const subtitle = tpl.image || (tpl.category ? tpl.category : 'sandbox')

  return (
    <div className="panel tplcard">
      <div className="tplcard-body">
        <div className="tplcard-head">
          <div className="ic tplcard-ic"><TemplateIcon t={tpl} /></div>
          <div className="tplcard-id">
            <div className="tplcard-name">
              <h2 style={{ margin: 0, fontSize: 15, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{tpl.name}</h2>
              {tpl.builtin && (
                <span style={{ fontSize: 10.5, fontWeight: 600, color: ORANGE, letterSpacing: '.04em', textTransform: 'uppercase' }}>catalog</span>
              )}
            </div>
            <div style={{ fontSize: 12, color: 'var(--dim)', fontFamily: 'var(--mono)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{subtitle}</div>
          </div>
        </div>

        {tpl.description && (
          <p style={{ margin: '10px 0 0', fontSize: 12.5, color: 'var(--sub)', lineHeight: 1.45 }}>{tpl.description}</p>
        )}

        <div className="tplcard-tags">
          {tpl.category ? <span className="chip">{tpl.category}</span> : null}
          {(tpl.ports ?? []).map(p => <span key={p} className="chip">:{p}</span>)}
          {tpl.env && tpl.env.length > 0 ? <span className="chip">{tpl.env.length} env</span> : null}
          <span className="chip">{tpl.cpu} vCPU</span>
          <span className="chip">{tpl.memory_mb} MB</span>
        </div>
      </div>

      <div className="tplcard-foot">
        <button className="btn primary tplcard-use" onClick={use}>
          Use template →
        </button>
      </div>
    </div>
  )
}

export default function Templates() {
  const [templates, setTemplates] = useState<Template[]>([])
  const [loadErr, setLoadErr] = useState('')
  const [loading, setLoading] = useState(true)

  function reload() {
    setLoading(true)
    listTemplates()
      .then(d => setTemplates(d ?? []))
      .catch(e => setLoadErr(String(e)))
      .finally(() => setLoading(false))
  }

  useEffect(reload, [])

  function catalog() {
    if (loadErr) return <ErrorState error={loadErr} onRetry={reload} title="Couldn’t load templates" />
    if (loading) return <div className="panel"><RowsSkeleton rows={3} /></div>
    if (templates.length === 0) return <div className="panel"><div className="empty">No templates available.</div></div>
    return (
      <div className="tpl-grid">
        {templates.map(tpl => <TemplateCard key={tpl.id} tpl={tpl} />)}
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Templates</h1>
          <p>
            Every environment you can launch.{' '}
            <Link to="/deploy">Open the catalog →</Link> to search and filter by category.
          </p>
        </div>
      </div>

      {catalog()}

      <div className="panel" style={{ marginTop: 20 }}>
        <div className="ph"><h2>Custom environments</h2></div>
        <div className="tpl-prose">
          <p style={{ margin: '0 0 10px' }}>
            To run something the catalog doesn’t cover, you have two options that
            actually persist:
          </p>
          <p style={{ margin: '0 0 10px' }}>
            <strong style={{ color: 'var(--fg)' }}>Use any public image directly.</strong>{' '}
            Pass <code>image</code> when you create a sandbox — no template needed:
          </p>
          <pre className="pre" style={{ margin: '0 0 14px' }}>
{`curl -X POST $API/v1/sandboxes \\
  -H "Authorization: Bearer $KEY" \\
  -d '{"template":"base","image":"myorg/tools:1.2","ports":[8080]}'`}
          </pre>
          <p style={{ margin: '0 0 10px' }}>
            <strong style={{ color: 'var(--fg)' }}>Or build and publish an image</strong>{' '}
            so it’s one click for the whole team. It builds on one node and is
            published to the fleet, so any node can run it; the result shows up
            under <Link to="/images">Images</Link>:
          </p>
          <pre className="pre" style={{ margin: 0 }}>
{`tar czf - . | curl -X POST "$API/v1/images/build?name=my-toolchain" \\
  -H "Authorization: Bearer $KEY" --data-binary @-

# {"ok":true,"tag":"sf-<org>-my-toolchain:latest",
#  "build":{"node":"node-a","ok":true},
#  "publish":{"ok":true,"build_id":"bld-…","template_id":"sf-<org>-my-toolchain"}}
# ok is true only when it built AND published. If publish.ok is false on a
# multi-node fleet, the image exists on one node only — read publish.error.`}
          </pre>
        </div>
      </div>
    </div>
  )
}

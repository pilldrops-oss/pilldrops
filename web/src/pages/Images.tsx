import { useEffect, useState } from 'react'
import { useToast } from '../components/Toast'
import { errText } from '../lib/errText'
import { listImages, patchImage } from '../api'
import type { ImageInfo } from '../types'

function Toggle({ on, label, busy, onClick }: { on: boolean; label: string; busy: boolean; onClick: () => void }) {
  return (
    <button
      className="btn"
      onClick={onClick}
      disabled={busy}
      aria-pressed={on}
      style={{
        padding: '4px 11px', fontSize: 12.5, fontWeight: 600,
        background: on ? 'var(--greensoft)' : 'var(--bg2)',
        color: on ? 'var(--green)' : 'var(--sub)',
        border: `1px solid ${on ? 'var(--greenline)' : 'var(--line)'}`,
      }}
    >
      <span style={{
        display: 'inline-block', width: 6, height: 6, borderRadius: '50%', marginRight: 6,
        background: on ? 'var(--green)' : 'var(--faint)',
      }} />
      {label}
    </button>
  )
}

function ImageCard({ img, onPatched }: { img: ImageInfo; onPatched: (updated: ImageInfo) => void }) {
  const toast = useToast()
  const [busy, setBusy] = useState('')

  async function toggle(field: 'published' | 'featured') {
    setBusy(field)
    try {
      const updated = await patchImage(img.id, { [field]: !img[field] })
      onPatched(updated)
    } catch (e) {
      toast.error(errText(e))
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="panel" style={{ display: 'flex', flexDirection: 'column' }}>
      <div style={{ padding: 16, flex: 1 }}>
        <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: 8 }}>
          <h2 style={{ margin: 0, fontSize: 15 }}>{img.name || img.id}</h2>
          <span style={{ fontSize: 12, color: 'var(--dim)', fontFamily: 'var(--mono)' }}>{img.id}</span>
        </div>
        <p style={{ margin: '8px 0 0', fontSize: 13, color: 'var(--sub)', lineHeight: 1.5 }}>{img.description}</p>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 7, marginTop: 12 }}>
          {img.port ? <span className="chip">:{img.port}</span> : null}
          <span className="chip">{img.vcpu} vCPU</span>
          <span className="chip">{img.memory_mb} MB</span>
          {img.size_mb ? <span className="chip">{(img.size_mb / 1024).toFixed(1)} GB</span> : null}
          {img.language ? <span className="chip">{img.language}</span> : null}
          {(img.tags ?? []).map(t => <span key={t} className="chip">{t}</span>)}
        </div>
      </div>
      <div style={{ padding: '0 16px 14px', display: 'flex', gap: 8 }}>
        <Toggle on={img.published} label="Published" busy={busy === 'published'} onClick={() => toggle('published')} />
        <Toggle on={img.featured} label="Featured" busy={busy === 'featured'} onClick={() => toggle('featured')} />
      </div>
    </div>
  )
}

export default function Images() {
  const [images, setImages] = useState<ImageInfo[]>([])
  const [loadErr, setLoadErr] = useState('')
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    listImages()
      .then(d => setImages(d.images ?? []))
      .catch(e => setLoadErr(String(e)))
      .finally(() => setLoading(false))
  }, [])

  function onPatched(updated: ImageInfo) {
    setImages(prev => prev.map(i => (i.id === updated.id ? updated : i)))
  }

  function grid() {
    if (loadErr) return <div className="panel"><div className="empty">Failed to load images: {loadErr}</div></div>
    if (loading) return <div className="panel"><div className="empty">Loading…</div></div>
    if (images.length === 0) return <div className="panel"><div className="empty">No images discovered yet.</div></div>
    return (
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 14 }}>
        {images.map(img => (
          <ImageCard key={img.id} img={img} onPatched={onPatched} />
        ))}
      </div>
    )
  }

  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Images</h1>
          <p>Curate which ready-made images customers can launch, and highlight featured ones.</p>
        </div>
      </div>

      <div className="est-hint" style={{ marginBottom: 16 }}>
        New images are published with <code>scripts/build-template.sh</code>; they appear here automatically.
        Toggle which ones customers can launch.
      </div>

      {grid()}
    </div>
  )
}

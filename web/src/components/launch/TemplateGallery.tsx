import { useState, useMemo, useEffect } from 'react'
import { Link } from 'react-router-dom'
import type { NodeInfo, Template } from '../../types'

import { TemplateIcon } from '../templateIcons'

import { track } from '../../lib/telemetry'
import { scoreTemplate, sortTemplates, SORT_LABELS, type SortKey } from '../../lib/templateSearch'

import { type Category, CATEGORY_ORDER, categoryOf, sizeLabel } from '../../lib/launchKinds'

export function groupByCategory(templates: Template[]): { category: Category; items: Template[] }[] {
  const byCat = new Map<Category, Template[]>()
  for (const t of templates) {
    const c = categoryOf(t)
    const arr = byCat.get(c) ?? []
    arr.push(t)
    byCat.set(c, arr)
  }
  return CATEGORY_ORDER.map(c => ({ category: c, items: byCat.get(c) ?? [] })).filter(
    g => g.items.length > 0,
  )
}

export function matchesQuery(t: Template, q: string): boolean {
  return scoreTemplate(t, q) > 0
}

export function quickLaunchable(t: Template): boolean {
  if ((t.env ?? []).some(e => e.required && !e.secret && !e.value)) return false
  const pi = t.primary_input
  return !pi || !pi.required
}

// bootableOn mirrors the scheduler's hostsTemplate: a healthy node boots a
// template it holds, or anything when it does not enumerate or can fetch.
// No node list (local, devserver, a key that may not read it) greys nothing.
export function bootableOn(nodes: NodeInfo[] | null): (id: string) => boolean {
  if (!nodes?.length) return () => true
  const healthy = nodes.filter(n => n.healthy)
  return id => healthy.some(n => n.templates == null || n.can_fetch_templates || n.templates.includes(id))
}

export function TemplateList({
  templates,
  selectedId,
  onSelect,
}: {
  templates: Template[]
  selectedId: string
  onSelect: (t: Template) => void
}) {
  const [query, setQuery] = useState('')

  const groups = useMemo(() => {
    const q = query.trim().toLowerCase()
    const filtered = q
      ? templates.filter(t =>
          [t.id, t.name ?? '', t.description ?? '', t.language ?? '', ...(t.tags ?? [])]
            .join(' ')
            .toLowerCase()
            .includes(q),
        )
      : templates
    const byCat = new Map<Category, Template[]>()
    for (const t of filtered) {
      const c = categoryOf(t)
      const arr = byCat.get(c) ?? []
      arr.push(t)
      byCat.set(c, arr)
    }
    return CATEGORY_ORDER.map(c => ({ category: c, items: byCat.get(c) ?? [] })).filter(
      g => g.items.length > 0,
    )
  }, [templates, query])

  return (
    <div className="lm-left">
      <input
        className="lm-search"
        value={query}
        onChange={e => setQuery(e.target.value)}
        placeholder="Search templates…"
        aria-label="Search templates"
      />
      <div className="lm-list" role="listbox" aria-label="Templates">
        {groups.length === 0 && <div className="lm-empty">No templates match “{query}”.</div>}
        {groups.map(g => (
          <div key={g.category} className="lm-group">
            <div className="lm-group-h">{g.category}</div>
            {g.items.map(t => (
              <button
                key={t.id}
                type="button"
                role="option"
                aria-selected={selectedId === t.id}
                className={`lm-item${selectedId === t.id ? ' selected' : ''}`}
                onClick={() => onSelect(t)}
              >
                <span className="lm-item-icon"><TemplateIcon t={t} /></span>
                <span className="lm-item-body">
                  <span className="lm-item-name">{t.name || t.id}</span>
                  <span className="lm-item-desc">{t.description}</span>
                </span>
              </button>
            ))}
          </div>
        ))}
      </div>
    </div>
  )
}

export function TemplateGallery({
  templates,
  onPick,
  onQuickLaunch,
  favorites,
  onToggleFavorite,
  bootable,
}: {
  templates: Template[]
  onPick: (t: Template) => void
  onQuickLaunch?: (t: Template, primaryValue?: string) => void
  favorites?: string[]
  onToggleFavorite?: (id: string) => void
  bootable?: (id: string) => boolean
}) {
  const [query, setQuery] = useState('')
  const [cat, setCat] = useState<'All' | Category>('All')
  const [sort, setSort] = useState<SortKey>('recommended')

  const availableCats = useMemo<('All' | Category)[]>(() => {
    const present = new Set(templates.map(categoryOf))
    return ['All', ...CATEGORY_ORDER.filter(c => present.has(c))]
  }, [templates])

  const groups = useMemo<{ category: string; items: Template[] }[]>(() => {
    const q = query.trim()
    const filtered = sortTemplates(
      templates.filter(t => (cat === 'All' || categoryOf(t) === cat) && matchesQuery(t, q)),
      sort,
      q,
    )
    const base: { category: string; items: Template[] }[] = groupByCategory(filtered)
    const favSet = new Set(favorites ?? [])
    const favItems = filtered.filter(t => favSet.has(t.id))
    return favItems.length ? [{ category: 'Favorites', items: favItems }, ...base] : base
  }, [templates, query, cat, sort, favorites])

  useEffect(() => {
    const q = query.trim()
    if (!q || groups.length > 0) return
    const t = window.setTimeout(() => track('template_search_empty', { query: q, category: cat }), 800)
    return () => window.clearTimeout(t)
  }, [query, groups.length, cat])

  return (
    <>
      <div className="dep-toolbar">
        <div className="dep-search">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
            <circle cx="11" cy="11" r="7" />
            <path d="m21 21-4.3-4.3" />
          </svg>
          <input
            value={query}
            onChange={e => setQuery(e.target.value)}
            placeholder="Search templates…"
            aria-label="Search templates"
          />
        </div>
        <div className="dep-filters" role="tablist" aria-label="Filter by category">
          {availableCats.map(c => (
            <button
              key={c}
              type="button"
              role="tab"
              aria-selected={cat === c}
              className={`dep-chip-f${cat === c ? ' on' : ''}`}
              onClick={() => setCat(c)}
            >
              {c}
            </button>
          ))}
        </div>
        <select
          className="sel"
          value={sort}
          onChange={e => setSort(e.target.value as SortKey)}
          aria-label="Sort templates"
        >
          {(Object.keys(SORT_LABELS) as SortKey[]).map(k => (
            <option key={k} value={k}>
              {SORT_LABELS[k]}
            </option>
          ))}
        </select>
      </div>

      {groups.length === 0 ? (
        <p className="dep-nomatch">No templates match this filter.</p>
      ) : (
        groups.map(g => (
          <div key={g.category}>
            <div className="dep-grp-title">{g.category}</div>
            <div className="dep-grid">
              {g.items.map(t => (
                <TemplateCard
                  key={t.id}
                  tpl={t}
                  onPick={onPick}
                  onQuickLaunch={onQuickLaunch}
                  fav={favorites?.includes(t.id)}
                  onToggleFavorite={onToggleFavorite}
                  off={bootable ? !bootable(t.id) : false}
                />
              ))}
            </div>
          </div>
        ))
      )}
    </>
  )
}

function TemplateCard({
  tpl,
  onPick,
  onQuickLaunch,
  fav,
  onToggleFavorite,
  off,
}: {
  tpl: Template
  onPick: (t: Template) => void
  onQuickLaunch?: (t: Template, primaryValue?: string) => void
  fav?: boolean
  onToggleFavorite?: (id: string) => void
  off?: boolean
}) {
  const langBadge = tpl.language || tpl.tags?.[0]
  const port = tpl.default_port ?? tpl.port
  const cpu = tpl.cpu || 1
  const zeroQuick = !off && !!onQuickLaunch && quickLaunchable(tpl)
  return (
    <div
      className={`dep-card${off ? ' off' : ''}`}
      role="button"
      tabIndex={0}
      aria-disabled={off || undefined}
      title={off ? 'No node in this fleet can boot this template' : undefined}
      onClick={() => { if (!off) onPick(tpl) }}
      onKeyDown={e => {
        if (!off && (e.key === 'Enter' || e.key === ' ')) {
          e.preventDefault()
          onPick(tpl)
        }
      }}
    >
      <div className="dep-card-head">
        <span className="dep-card-ic"><TemplateIcon t={tpl} /></span>
        <div className="dep-card-title">
          <span className="dep-card-nm">{tpl.name || tpl.id}</span>
          {langBadge && <span className="dep-badge">{langBadge}</span>}
        </div>
        {onToggleFavorite && (
          <button
            type="button"
            className={`dep-card-fav${fav ? ' on' : ''}`}
            aria-label={fav ? 'Unpin from favorites' : 'Pin to favorites'}
            aria-pressed={fav}
            title={fav ? 'Unpin from favorites' : 'Pin to favorites'}
            onClick={e => {
              e.stopPropagation()
              onToggleFavorite(tpl.id)
            }}
          >
            {fav ? '★' : '☆'}
          </button>
        )}
      </div>
      <div className="dep-card-desc">{tpl.description}</div>
      <Link
        to={`/t/${tpl.id}`}
        className="dep-card-details"
        onClick={e => e.stopPropagation()}
        onKeyDown={e => e.stopPropagation()}
      >
        Details →
      </Link>
      {tpl.preinstalled && tpl.preinstalled.length > 0 && (
        <div className="dep-inside">
          {tpl.preinstalled.slice(0, 3).map(p => (
            <span key={p} className="dep-inside-i">{p}</span>
          ))}
          {tpl.preinstalled.length > 3 && (
            <span className="dep-inside-more">+{tpl.preinstalled.length - 3}</span>
          )}
        </div>
      )}
      <div className="dep-specs">
        <span className="chip">{cpu} vCPU · {sizeLabel(tpl.memory_mb || 512)}</span>
        {tpl.has_server && port ? (
          <span className="chip dep-chip-g">Opens a web app</span>
        ) : null}
        {off && <span className="chip">No node can boot this</span>}
      </div>
      {zeroQuick && (
        <div
          className="dep-quick"
          onClick={e => e.stopPropagation()}
          onKeyDown={e => e.stopPropagation()}
        >
          <button type="button" className="dep-quick-btn" onClick={() => onQuickLaunch!(tpl)}>
            ⚡ Quick launch
          </button>
          <span className="dep-quick-note">template defaults · click the card to configure</span>
        </div>
      )}
    </div>
  )
}

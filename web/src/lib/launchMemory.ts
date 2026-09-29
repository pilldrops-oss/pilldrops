// localStorage memory of recents, presets and favourite templates.

import type { CreateSandboxReq } from '../types'

export interface StoredLaunch {
  template: string
  /** Display name at launch time; template names can change. */
  name: string
  /** Epoch ms. */
  at: number
  /** The exact create request, replayed by Relaunch; omitted when too large to store. */
  body?: CreateSandboxReq
}

export interface LaunchPreset extends StoredLaunch {
  label: string
}

const RECENTS_KEY = 'pd-recent-launches'
const PRESETS_KEY = 'pd-launch-presets'
const FAVS_KEY = 'pd-fav-templates'
const MAX_RECENTS = 6
// Larger bodies (project uploads) are stored body-less; localStorage quotas are small.
const MAX_BODY_JSON = 100_000

function read<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key)
    return raw ? (JSON.parse(raw) as T) : fallback
  } catch {
    return fallback
  }
}

function write(key: string, value: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(value))
  } catch {
    // Quota and private-mode failures are non-fatal.
  }
}

export function listRecents(): StoredLaunch[] {
  return read<StoredLaunch[]>(RECENTS_KEY, [])
}

export function recordRecent(entry: StoredLaunch) {
  const e = { ...entry }
  if (e.body && JSON.stringify(e.body).length > MAX_BODY_JSON) delete e.body
  // One slot per template.
  const rest = listRecents().filter(r => r.template !== e.template)
  write(RECENTS_KEY, [e, ...rest].slice(0, MAX_RECENTS))
}

export function listPresets(): LaunchPreset[] {
  return read<LaunchPreset[]>(PRESETS_KEY, [])
}

export function savePreset(preset: LaunchPreset) {
  const rest = listPresets().filter(p => p.label !== preset.label)
  write(PRESETS_KEY, [preset, ...rest])
}

export function deletePreset(label: string) {
  write(PRESETS_KEY, listPresets().filter(p => p.label !== label))
}

export function listFavorites(): string[] {
  return read<string[]>(FAVS_KEY, [])
}

export function toggleFavorite(id: string): string[] {
  const favs = listFavorites()
  const next = favs.includes(id) ? favs.filter(f => f !== id) : [...favs, id]
  write(FAVS_KEY, next)
  return next
}

export function timeAgo(at: number): string {
  const s = Math.max(1, Math.floor((Date.now() - at) / 1000))
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

// Persists the Build conversation in localStorage (fast path, read at mount)
// with a best-effort server copy behind it. What is stored is a transcript,
// not a live status.

import { createThread, patchThread, ApiError } from '../api'

const KEY = 'pilldrops.build.thread'
// Kept separate from the transcript so a failed sync never corrupts it.
const ID_KEY = 'pilldrops.build.thread.id'
const MAX_TURNS = 60

export interface StoredThread {
  at: number
  /** Opaque to this module; the page owns the turn shape. */
  turns: unknown[]
}

/** saveThread stores the transcript. Silent on storage failure; a build must not be interrupted by its diary. */
export function saveThread(turns: unknown[]): void {
  try {
    if (turns.length === 0) {
      localStorage.removeItem(KEY)
      return
    }
    const payload: StoredThread = { at: Date.now(), turns: turns.slice(-MAX_TURNS) }
    localStorage.setItem(KEY, JSON.stringify(payload))
  } catch {
    /* storage unavailable */
  }
  // Local must land first and synchronously; it is what the page reads at mount.
  syncToServer(turns)
}

/** activeThreadId returns the server thread the local transcript belongs to, or null. */
export function activeThreadId(): string | null {
  try {
    return localStorage.getItem(ID_KEY)
  } catch {
    return null
  }
}

function setActiveThreadId(id: string | null): void {
  try {
    if (id === null) localStorage.removeItem(ID_KEY)
    else localStorage.setItem(ID_KEY, id)
  } catch {
    /* no storage */
  }
}

/** threadTitle names a conversation from its first user message. */
export function threadTitle(turns: unknown[]): string {
  for (const t of turns) {
    const turn = t as { kind?: unknown; text?: unknown }
    if (turn?.kind === 'you' && typeof turn.text === 'string' && turn.text.trim()) {
      return turn.text.trim().slice(0, 80)
    }
  }
  return ''
}

// One sync at a time, latest transcript wins. Firing per save would race a
// create against its own patch and produce duplicate threads.
let syncing = false
let pendingTurns: unknown[] | null = null

function syncToServer(turns: unknown[]): void {
  pendingTurns = turns
  if (syncing) return
  syncing = true
  void (async () => {
    while (pendingTurns !== null) {
      const batch = pendingTurns
      pendingTurns = null
      try {
        const body = JSON.stringify(batch.slice(-MAX_TURNS))
        const id = activeThreadId()
        if (id) {
          await patchThread(id, { turns: body })
        } else {
          const created = await createThread(threadTitle(batch), body)
          setActiveThreadId(created.id)
        }
      } catch (e) {
        // 404 means deleted elsewhere, so the next save creates a new thread.
        // Other errors keep the id for when the server comes back.
        if (e instanceof ApiError && e.status === 404) setActiveThreadId(null)
      }
    }
    syncing = false
  })()
}

/**
 * switchToThread makes a server thread the local one and re-seeds subscribed
 * views. An unparsable transcript opens empty rather than crashing.
 */
export function switchToThread(id: string, turnsJson: string, updatedAt: number): void {
  let turns: unknown[] = []
  try {
    const parsed = JSON.parse(turnsJson) as unknown
    if (Array.isArray(parsed)) turns = parsed
  } catch {
    /* open empty */
  }
  try {
    if (turns.length === 0) localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, JSON.stringify({ at: updatedAt * 1000, turns } satisfies StoredThread))
  } catch {
    /* no storage */
  }
  setActiveThreadId(id)
  for (const fn of [...switched]) {
    try {
      fn()
    } catch {
      /* one broken view must not strand the others */
    }
  }
}

type SwitchedListener = () => void
const switched = new Set<SwitchedListener>()

/** onThreadSwitched subscribes to thread switches and returns an unsubscribe. */
export function onThreadSwitched(fn: SwitchedListener): () => void {
  switched.add(fn)
  return () => switched.delete(fn)
}

/** loadThread reads the transcript back; anything malformed degrades to null. */
export function loadThread(): StoredThread | null {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw) as unknown
    if (
      !parsed ||
      typeof parsed !== 'object' ||
      !Array.isArray((parsed as StoredThread).turns) ||
      typeof (parsed as StoredThread).at !== 'number'
    ) {
      return null
    }
    const t = parsed as StoredThread
    return t.turns.length ? t : null
  } catch {
    return null
  }
}

// Clearing is an event: Build is the index route, so navigating to it does not
// remount, and a view seeded at mount would keep showing the dead transcript.
type ClearedListener = () => void
const cleared = new Set<ClearedListener>()

/** onThreadCleared subscribes to thread clears and returns an unsubscribe. */
export function onThreadCleared(fn: ClearedListener): () => void {
  cleared.add(fn)
  return () => cleared.delete(fn)
}

export function clearThread(): void {
  try {
    localStorage.removeItem(KEY)
  } catch {
    /* nothing to do */
  }
  // Detach, do not delete: the old conversation stays on the server.
  setActiveThreadId(null)
  // Notify even if the write failed; a view still showing the thread is the bug either way.
  for (const fn of [...cleared]) {
    try {
      fn()
    } catch {
      /* one broken view must not strand the others */
    }
  }
}

/**
 * describeAge frames a restored transcript's age, so a stale "serving" claim
 * does not read as live status.
 */
export function describeAge(at: number, now: number): string {
  const mins = Math.max(0, Math.round((now - at) / 60_000))
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.round(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  return `${Math.round(hrs / 24)}d ago`
}

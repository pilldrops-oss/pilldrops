import { createContext, useContext, useState, useCallback, useRef, type ReactNode } from 'react'

export type ToastKind = 'success' | 'error' | 'info'

const TOAST_ICON: Partial<Record<ToastKind, string>> = { success: '✓', error: '!' }

export interface ToastAction { label: string; to: string }

interface ToastItem { id: number; kind: ToastKind; message: string; action?: ToastAction }
interface ToastOptions { kind?: ToastKind; action?: ToastAction; ttl?: number }

interface ToastApi {
  show: (message: string, opts?: ToastOptions) => void
  success: (message: string, opts?: ToastOptions) => void
  error: (message: string, opts?: ToastOptions) => void
}

const ToastCtx = createContext<ToastApi | null>(null)

export function useToast(): ToastApi {
  const ctx = useContext(ToastCtx)
  if (!ctx) throw new Error('useToast must be used within <ToastProvider>')
  return ctx
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const nextId = useRef(1)

  const dismiss = useCallback((id: number) => {
    setItems(list => list.filter(t => t.id !== id))
  }, [])

  const show = useCallback((message: string, opts: ToastOptions = {}) => {
    const id = nextId.current++
    const kind = opts.kind ?? 'info'
    const ttl = opts.ttl ?? (kind === 'error' ? 6500 : 4000)
    setItems(list => [...list, { id, kind, message, action: opts.action }])
    if (ttl > 0) setTimeout(() => dismiss(id), ttl)
  }, [dismiss])

  const api: ToastApi = {
    show,
    success: (m, o) => show(m, { ...o, kind: 'success' }),
    error: (m, o) => show(m, { ...o, kind: 'error' }),
  }

  return (
    <ToastCtx.Provider value={api}>
      {children}
      <Toaster items={items} onDismiss={dismiss} />
    </ToastCtx.Provider>
  )
}

function Toaster({ items, onDismiss }: { items: ToastItem[]; onDismiss: (id: number) => void }) {
  if (items.length === 0) return null
  return (
    <div className="toaster" role="region" aria-live="polite" aria-label="Notifications">
      {items.map(t => (
        <div key={t.id} className={`toast ${t.kind}`} role="status">
          <span className="toast-icon" aria-hidden="true">
            {TOAST_ICON[t.kind] ?? 'i'}
          </span>
          <span className="toast-msg">{t.message}</span>
          {t.action && (
            <button
              className="toast-action"
              onClick={() => { window.location.hash = `#/${t.action!.to.replace(/^#?\/?/, '')}`; onDismiss(t.id) }}
            >
              {t.action.label}
            </button>
          )}
          <button className="toast-x" aria-label="Dismiss notification" onClick={() => onDismiss(t.id)}>×</button>
        </div>
      ))}
    </div>
  )
}

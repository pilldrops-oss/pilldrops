import { createContext, useContext, useState, useCallback, useEffect, type ReactNode } from 'react'

export interface ConfirmOptions {
  title: string
  body?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  danger?: boolean
}

type ConfirmFn = (opts: ConfirmOptions) => Promise<boolean>

const ConfirmCtx = createContext<ConfirmFn | null>(null)

export function useConfirm(): ConfirmFn {
  const ctx = useContext(ConfirmCtx)
  if (!ctx) throw new Error('useConfirm must be used within <ConfirmProvider>')
  return ctx
}

interface Pending extends ConfirmOptions { resolve: (v: boolean) => void }

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<Pending | null>(null)

  const confirm = useCallback<ConfirmFn>((opts) => {
    return new Promise<boolean>(resolve => setPending({ ...opts, resolve }))
  }, [])

  const close = useCallback((v: boolean) => {
    setPending(p => { p?.resolve(v); return null })
  }, [])

  useEffect(() => {
    if (!pending) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(false)
      // A destructive dialog leaves Enter to the focused button, which starts on Cancel.
      else if (e.key === 'Enter' && !pending.danger) close(true)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [pending, close])

  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      {pending && (
        <div className="overlay" onMouseDown={() => close(false)}>
          <div
            className="modal confirm-modal"
            onMouseDown={e => e.stopPropagation()}
            role="alertdialog"
            aria-modal="true"
          >
            <h2>{pending.title}</h2>
            {pending.body && <div className="confirm-body">{pending.body}</div>}
            <div className="confirm-actions">
              {/* A destructive action starts on Cancel, so a stray Enter does no harm. */}
              <button className="btn" onClick={() => close(false)} autoFocus={pending.danger}>{pending.cancelLabel ?? 'Cancel'}</button>
              <button
                className={`btn ${pending.danger ? 'destructive' : 'primary'}`}
                onClick={() => close(true)}
                autoFocus={!pending.danger}
              >
                {pending.confirmLabel ?? 'Confirm'}
              </button>
            </div>
          </div>
        </div>
      )}
    </ConfirmCtx.Provider>
  )
}

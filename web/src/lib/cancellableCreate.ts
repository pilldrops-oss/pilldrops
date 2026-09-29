import type { CreateSandboxResp } from '../types'

export type CreateOutcome =
  | { cancelled: false; resp: CreateSandboxResp }
  | { cancelled: true }

export interface CancellableCreate {
  /** Resolves as soon as the create finishes OR the caller cancels. */
  result: Promise<CreateOutcome>
  cancel: () => void
}

// cancellableCreate runs a create the user can walk away from. The server call cannot be
// aborted, so a cancelled create destroys whatever it eventually produces, or it would bill.
export function cancellableCreate(opts: {
  create: () => Promise<CreateSandboxResp>
  destroy: (id: string) => Promise<unknown>
}): CancellableCreate {
  let cancelled = false
  let onCancel: (() => void) | null = null

  const cancellation = new Promise<CreateOutcome>(resolve => {
    onCancel = () => resolve({ cancelled: true })
  })

  const inFlight = opts.create().then(
    resp => {
      if (cancelled) {
        void Promise.resolve(opts.destroy(resp.id)).catch(() => {})
        return { cancelled: true } as CreateOutcome
      }
      return { cancelled: false, resp } as CreateOutcome
    },
    err => {
      // Nobody is watching a cancelled create; do not surface its failure.
      if (cancelled) return { cancelled: true } as CreateOutcome
      throw err
    },
  )

  return {
    result: Promise.race([inFlight, cancellation]),
    cancel: () => {
      cancelled = true
      onCancel?.()
    },
  }
}

export type LifetimeKey = 'idle' | '1h' | '8h' | 'forever'

export const LIFETIME_CHOICES: Array<{ key: LifetimeKey; label: string; note: string }> = [
  { key: 'idle', label: 'Stop when idle', note: 'Cheapest. Pauses once nothing is using it.' },
  { key: '1h', label: 'Keep alive 1 hour', note: 'Preview links keep working for an hour.' },
  { key: '8h', label: 'Keep alive 8 hours', note: 'A working day.' },
  { key: 'forever', label: 'Until I stop it', note: 'Runs until you pause or destroy it. Bills the whole time.' },
]

/** lifetimeToInputs maps a choice to resolveLifetime() inputs; empty strings mean "use the default". */
export function lifetimeToInputs(key: LifetimeKey): { timeoutInput: string; idleInput: string } {
  switch (key) {
    case '1h': return { timeoutInput: '3600', idleInput: '-1' }
    case '8h': return { timeoutInput: '28800', idleInput: '-1' }
    case 'forever': return { timeoutInput: '-1', idleInput: '-1' }
    case 'idle': return { timeoutInput: '', idleInput: '' }
  }
}

/** formatCountdown returns time remaining as a duration; `expiresAtSec <= 0` means no expiry. */
export function formatCountdown(expiresAtSec: number, nowSec: number): string {
  if (!expiresAtSec || expiresAtSec <= 0) return ''
  const left = expiresAtSec - nowSec
  if (left <= 0) return 'expired'
  if (left < 60) return `${left}s`
  const m = Math.floor(left / 60)
  if (m < 60) return `${m}m`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

/** EventName lists the product events; keep it short, each answers a specific question. */
export type EventName =
  /** A gallery search returned nothing. */
  | 'template_search_empty'
  /** Final outcome of a deploy; the source for launch-success rate. */
  | 'launch_outcome'
  /** A prompt-first request hit /v1/ai/configure. Never carries the prompt text, only its length. */
  | 'ai_plan_requested'
  /** How a prompt-first plan concluded: launched, handed to the manual form, or re-templated. */
  | 'ai_plan_outcome'

type Sink = (name: EventName, props: Record<string, unknown>) => void

let sink: Sink | null = null

/** setSink wires the analytics transport once at app startup. */
export function setSink(s: Sink): void {
  sink = s
}

/** Test-only sink override. */
export function __setSink(s: Sink | null): void {
  sink = s
}

/** track records an event. Never throws; telemetry must not break a deploy. */
export function track(name: EventName, props: Record<string, unknown> = {}): void {
  try {
    sink?.(name, props)
  } catch {
    /* best-effort */
  }
}

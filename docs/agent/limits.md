# Agent limits, retention and rate limits

> **Generated file — do not edit.** Rendered from the constants the server enforces by
> `cmd/gendocs`. Run `make docs` after changing the source; a test fails if this
> file is stale. Editing it by hand means the next regenerate silently discards
> your words.

## Event retention

| What | Kept for | Notes |
|---|---|---|
| Journal events in the sandbox | Until the sandbox is destroyed | The journal lives on the workstation's disk, which is in its snapshot — so pause and resume keep it. |
| Mirrored events in the control plane | **30 days** (`AGENT_EVENT_RETENTION_DAYS`) | Set per deployment. `0` disables pruning entirely rather than meaning "delete everything" — the reading that would lose a customer's history to a typo. |
| Mirrored **turns** in the control plane | Not pruned | Turns are the record of what the agent did and what it cost. Events are the detail of how; turns are the fact that it happened. |

Retention applies to the control plane's copy only. Reading history from a **running**
workstation always goes to the sandbox itself, so a short retention window never shortens what a
live machine can tell you.

## Output truncation

A single turn's output is capped in the journal. When the cap is hit, a
`turn.output_truncated` event is emitted and clients must surface it — a transcript that
simply ends looks complete, and that is the one failure mode worse than showing less.

## Webhook delivery

| Limit | Value | Why |
|---|---|---|
| Attempts per delivery | 6 | Full-jitter exponential backoff between them. |
| Consecutive failures before auto-disable | 20 | Consecutive, not cumulative: 20 failures over a year is a flaky receiver, 20 in a row is an endpoint that has gone away. |
| One attempt's timeout | 5s | A receiver that cannot answer in that time would otherwise become our queue depth. |
| Signature timestamp tolerance | 5m0s | Reject anything outside it. The signature covers the timestamp, so a receiver that does not check freshness gains nothing from the signing. |
| Test sends | 5 per minute per hook | This endpoint makes our server call an address you chose; without a budget it is an amplifier. |

A `4xx` other than `408` or `429` stops retrying immediately: the
receiver understood the request and refused it, so repeating it just delivers the same refusal
five more times.

**Not every event is deliverable.** `turn.output` is refused — it is the agent's raw
output stream, flushed several times a second, and forwarding it would flood an endpoint that
signed up for notifications. Subscribe to `turn.completed` and read the transcript. The
deliverable set is returned by the webhook list endpoint, from the same predicate the sender
enforces, so a client cannot offer an event that will never arrive.

## Queue and approvals

| Limit | Value | Why |
|---|---|---|
| Approval timeout | 15 minutes | **A timeout is a denial.** Silence is never taken as consent. |
| Pending approvals per sandbox | 32 | A bounded queue: an agent that parks thousands of calls would otherwise exhaust memory in the sandbox, and no human is going to review a thousand prompts. |
| Resolved approvals kept in memory | 200 | The durable record is in the control plane; this is the recent window a UI renders. |

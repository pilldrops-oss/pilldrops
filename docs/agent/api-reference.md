# Agent API reference

> **Generated file — do not edit.** Rendered from internal/httpapi/openapi.json by
> `cmd/gendocs`. Run `make docs` after changing the source; a test fails if this
> file is stale. Editing it by hand means the next regenerate silently discards
> your words.

Every endpoint below is scoped to one sandbox whose template declares `kind: agent`.
Any other sandbox answers **404**, deliberately indistinguishable from a sandbox that does not
exist — so this surface cannot be used to work out which templates an account runs.

Authentication is either an org API key or an **agent access token** (`sfa_…`), which
carries a narrow set of scopes and is bound to one sandbox. See the token guide for which scope
each endpoint needs and why no scope implies another.

## Contents

- [Access tokens and sharing](#access-tokens-and-sharing)
- [Approvals (Guided mode)](#approvals-guided-mode)
- [Connect and isolation](#connect-and-isolation)
- [Event stream](#event-stream)
- [Models and threads](#models-and-threads)
- [OpenAI-compatible adapter](#openai-compatible-adapter)
- [Session](#session)
- [Spend](#spend)
- [Turns and the queue](#turns-and-the-queue)
- [Webhooks](#webhooks)

## Access tokens and sharing

### `GET /agent/claim`

**Exchange a one-time share code for a session cookie (node-direct)**

The same handler without the sandbox in the path. Reachable only on the node itself and on the per-sandbox agent origin, where the host already carries the id — not through the control plane, which routes by path. Share links use the /v1/sandboxes/{id}/agent/claim form instead.

Parameters:

- `code` (query) *(required)*

Responses:

- **303** — Cookie set; redirected to the agent view
- **401** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/claim`

**Exchange a one-time share code for a session cookie**

Unauthenticated by necessity — the code IS the credential, which is why it is single-use and expires in minutes. This is the form share links use, because it is the only one the control plane can route: a proxy hop finds the owning node from the {id} in the path.

On success sets an HttpOnly cookie scoped to /v1/sandboxes/{id}/agent and redirects (303) to a URL with no credential in it. Used, expired, unknown, and path-mismatched codes all return the same 401, and the code is consumed either way, so a stale or leaked link cannot be probed.

Parameters:

- `code` (query) *(required)*

Responses:

- **303** — Cookie set; redirected to the agent view
- **401** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/share`

**Create a one-time share link for the agent**

Mints a read-scoped token (by default) and returns a claim_url containing a single-use code — not the token.

URLs end up in chat scrollback, browser history, referrer headers and access logs, none of which anyone audits, so a link must not be a durable credential. Opening claim_url exchanges the code for an HttpOnly cookie scoped to this sandbox's agent API and redirects to an address carrying nothing secret. The code cannot be reused.

A share link may not carry agent:console (400): a shell must not be reachable from something forwardable.

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Body:

- `label` (string)
- `scopes` (array) — Defaults to [agent:read]. agent:console is refused.
- `ttl_seconds` (integer)

Responses:

- **201** — The token metadata, a one-time claim_url and its expiry
- **400** — error, with a machine-readable `code` and a message written for a person
- **401** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/share/qr`

**QR payload for a share link**

Returns the string to render as a QR, client-side. It carries the single-use share link and NEVER an API token: a QR is a screenshot-shaped secret — it gets photographed, pasted into tickets and shown on slides, and none of those paths is auditable or revocable. Credential-shaped input is refused with 400 regardless of what the caller intended.

Parameters:

- `url` (query) *(required)* — The claim_url from POST /agent/share.

Responses:

- **200** — The QR payload
- **400** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/tokens`

**List agent access tokens**

Every token for this sandbox, newest first, including revoked and expired ones — "was there a credential that could have done this, and who made it" is the question asked after an incident.

Secrets are never included; there is no stored secret to return. The response also carries the scope catalogue so a permission chooser renders from the server that enforces it.

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Responses:

- **200** — Tokens and the scope catalogue
- **401** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/tokens`

**Mint an agent access token**

Creates a credential bound to THIS sandbox with an explicit scope set, for a teammate, a CI job or an MCP client that should not hold organization-wide authority.

The secret is returned exactly once — only its SHA-256 is stored, so it cannot be recovered later, only rotated.

Requires an organization API key. An agent access token cannot mint another, even with agent:admin: a credential that can issue its own successors is not revocable in practice, because revoking it leaves the children behind.

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Body:

- `label` (string) — Human label, shown in the token list.
- `scopes` (array) *(required)* — Scopes to grant. One of agent:read, agent:write, agent:console, agent:approve, agent:files, agent:admin. An unknown scope is rejected rather than ignored: a scope silently dropped would show as granted and behave as denied.
- `ttl_seconds` (integer) — Lifetime in seconds. Defaults to 7 days and is clamped to 30 days — an unbounded token is one that outlives the reason it was created.

Responses:

- **201** — The token metadata plus the one-time secret
- **400** — error, with a machine-readable `code` and a message written for a person
- **401** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `DELETE /v1/sandboxes/{id}/agent/tokens/{tokenID}`

**Revoke an agent access token**

Takes effect on the next request — nothing caches the decision, which is the whole reason these are opaque database-backed tokens rather than self-verifying JWTs.

Idempotent: revoking an already-revoked token succeeds and keeps the original timestamp, because the first revocation is the one an audit cares about.

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Parameters:

- `tokenID` (path) *(required)* — The token's public id. Safe to log and display; it is not the secret.

Responses:

- **200** — Revoked
- **401** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/tokens/{tokenID}/rotate`

**Rotate an agent access token**

Issues a new secret with the same sandbox, scopes and label, and shortens the previous token's life to a bounded overlap so requests already in flight with the old value are not broken.

The old token is expired rather than revoked, so "this was rotated out" stays distinguishable from "someone revoked this" — different incidents with different responses. Rotation never extends a token that was about to expire, and a revoked token cannot be rotated (409): that would resurrect access someone removed on purpose.

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Parameters:

- `tokenID` (path) *(required)* — The token's public id. Safe to log and display; it is not the secret.

Responses:

- **200** — The replacement token and its one-time secret
- **401** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — error, with a machine-readable `code` and a message written for a person

## Approvals (Guided mode)

### `GET /v1/sandboxes/{id}/agent/approvals`

**Pending tool approvals (Guided mode)**

Tool calls the agent has parked for review, plus recently resolved ones. **501 with a reason is NOT the same as an empty list**: an agent with no verified approval gate cannot park anything, and a client that showed "nothing to review" for it would be asserting something false about what is happening. Requires the `read` scope.

Responses:

- **200** — Pending and resolved approvals, the remember scopes on offer, the timeout, and whether THIS caller may decide
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/approvals/{approvalID}`

**Decide one approval**

Allow or deny a parked tool call. `remember` widens the decision to a category — `once`, `session` or `sandbox` — and the approval's `signature` is the category it would apply to, which is why a UI must show that signature next to the choice. **Not answering is a denial**: an approval that reaches its timeout is refused, so silence is never taken as consent. Requires the `approve` scope, which no other scope implies.

Parameters:

- `approvalID` (path) *(required)*

Body:

- `decision` (string) *(required)* — one of `allow`, `deny`
- `remember` (string) — one of `once`, `session`, `sandbox`. Omitted means once. Anything wider consents to the approval's whole signature, not just this call.

Responses:

- **200** — The decision as recorded, with the scope it was remembered at
- **400** — error, with a machine-readable `code` and a message written for a person
- **403** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — Already decided, or the approval timed out while the request was in flight

### `GET /v1/sandboxes/{id}/agent/policy`

**The allowlist and what has been remembered**

What runs without asking in Guided mode: the allowlist, plus every remembered grant with the scope it was granted at. Session grants are listed too — a grant that silently permits calls while being unlistable is a permission nobody can audit or revoke. Requires the `read` scope.

Responses:

- **200** — Allowlist, remembered grants, session grants
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `DELETE /v1/sandboxes/{id}/agent/policy/remembered`

**Forget a remembered grant**

Revokes one remembered allow by its signature, so a category consented to earlier stops applying. Takes `signature` as a query parameter — the exact string the policy and approval responses show, because revoking is only trustworthy if it names the same thing the grant did. Requires the `approve` scope.

Parameters:

- `signature` (query) *(required)*

Responses:

- **200** — The grant that was forgotten
- **404** — No such remembered grant
- **501** — error, with a machine-readable `code` and a message written for a person

## Connect and isolation

### `GET /v1/sandboxes/{id}/agent/connect`

**Everything needed to drive this agent, in one call**

Endpoints, a freshly minted short-lived token, copy-paste snippets (curl/Node/Python), an MCP client config, channel reachability and the per-sandbox agent origin.

The token is scoped to agent:read + agent:write only — never console (a shell behind a link people paste into terminals) and never admin (which could raise its own spend cap) — and is shown ONCE. A later call mints a different token; there is no way to retrieve this one's secret again.

Unavailable channels are listed with a reason rather than hidden, so "can I drive this from Slack?" gets an answer.

Responses:

- **200** — The connect bundle
- **404** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/isolation`

**What isolation this workstation actually has**

Each line is MEASURED, not asserted: the runtime that boots this sandbox, whether this request arrived over TLS, whether a provider credential is stored, and the autonomy the supervisor is really running in. `state` is `yes`/`no`/**`unknown`** — three values, because a boolean forces an unverifiable property to report as false, which is a claim of its own. Reports which env NAMES hold credentials, never their values. Requires the `read` scope.

Responses:

- **200** — Isolation facts, each with a state and an explanation
- **404** — error, with a machine-readable `code` and a message written for a person

## Event stream

### `GET /v1/sandboxes/{id}/agent/events`

**Stream agent events (SSE)**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates. Server-Sent Events. Resume with since_seq, or let a browser reconnect automatically via Last-Event-ID — either way you receive only what you missed rather than the run replayed from its start.

Parameters:

- `since_seq` (query) — Deliver events after this journal sequence number
- `Last-Event-ID` (header) — Resume cursor; takes precedence over since_seq

Responses:

- **200** — text/event-stream of journal events
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person
- **503** — error, with a machine-readable `code` and a message written for a person

## Models and threads

### `GET /v1/sandboxes/{id}/agent/models`

**Which models this agent can run**

The models the agent's CLI accepts, and which one it is currently using. Reported by the agent rather than listed here, so a provider adding a model does not require a change to this platform. Requires the `read` scope.

Responses:

- **200** — Available models and the current selection
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — This agent's CLI cannot enumerate models

### `GET /v1/sandboxes/{id}/agent/threads`

**Conversations on this workstation**

Threads derived from OUR records rather than by asking the CLI: only two of the supported agents can list their own sessions, so a thread API built on the CLI would exist for some agents and not others. An agent with no resume command reports no thread id rather than a thread that silently forgets. Requires the `read` scope.

Responses:

- **200** — Threads with their turn counts and last activity
- **404** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/threads/{thread}`

**One conversation's turns**

Every turn in one thread, oldest first. Requires the `read` scope.

Parameters:

- `thread` (path) *(required)*

Responses:

- **200** — The thread's turns
- **404** — No such thread for this sandbox

## OpenAI-compatible adapter

### `POST /v1/sandboxes/{id}/agent/responses`

**OpenAI-compatible Responses adapter**

Accepts an OpenAI Responses-shaped request and runs it as a turn, so an existing client can drive this agent without knowing the native API. A compatibility surface, not a second product: anything it cannot express maps onto the native turn semantics or is refused rather than approximated. Requires the `write` scope.

Body:

- `input` (string) — The task, in the OpenAI field name.
- `model` (string)
- `stream` (boolean) — Stream deltas instead of returning once the turn ends.

Responses:

- **200** — A Responses-shaped result
- **202** — Accepted when streaming
- **400** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — The agent is busy with another turn

### `GET /v1/sandboxes/{id}/agent/responses/{response}`

**Fetch one adapter response**

The Responses-shaped view of a turn. Requires the `read` scope.

Parameters:

- `response` (path) *(required)*

Responses:

- **200** — The response object
- **404** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/responses/{response}/cancel`

**Cancel an adapter response**

Interrupts the underlying turn. Partial output already journaled is kept — an interrupted turn that produced work should show how far it got rather than discarding it. Requires the `write` scope.

Parameters:

- `response` (path) *(required)*

Responses:

- **200** — Cancelled
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — Already finished

### `GET /v1/sandboxes/{id}/agent/responses/{response}/stream`

**Stream one adapter response**

SSE, in the Responses event shape. Resumes with `Last-Event-ID` like the native event stream, so a dropped connection does not lose the middle of a turn. Requires the `read` scope.

Parameters:

- `response` (path) *(required)*

Responses:

- **200** — SSE stream
- **404** — error, with a machine-readable `code` and a message written for a person

## Session

### `GET /v1/sandboxes/{id}/agent/status`

**Agent session status**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Responses:

- **200** — Session state, mode, current turn, queue depth and journal cursor
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

## Spend

### `GET /v1/sandboxes/{id}/agent/budget`

**Spend cap and spend to date**

The cap, what has been spent against it, and **`turns_unreported`** — turns whose agent reported no cost. That last field is the honest qualifier on the total: only some agents report usage, so a spend figure without it reads as complete when it is not. Requires the `read` scope.

Responses:

- **200** — Cap, spent, remaining, and the unreported-turn count
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — This agent has no budget file

### `PATCH /v1/sandboxes/{id}/agent/budget`

**Change the spend cap**

Sets the daily cap in USD. Requires the `admin` scope specifically — a token that could raise its own cap has no cap, so `write` deliberately does not imply this.

Body:

- `cap_usd` (number) *(required)*

Responses:

- **200** — The new cap
- **400** — error, with a machine-readable `code` and a message written for a person
- **403** — This token lacks the admin scope
- **404** — error, with a machine-readable `code` and a message written for a person

## Turns and the queue

### `POST /v1/sandboxes/{id}/agent/queue/reorder`

**Reorder the pending queue**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Body:

- `order` (array) — Turn ids first; unlisted turns keep their relative order behind them

Responses:

- **200** — Reordered
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/turns`

**List turns**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Responses:

- **200** — Turns, newest first
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/turns`

**Give the agent a task**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates. Returns 202: the task is accepted, not finished. Stream its progress from the returned cursor via the events endpoint.

Body:

- `message` (string) *(required)* — The task, in natural language
- `mode` (string) — one of `supervised`, `guided`, `autonomous`. Autonomy posture. guided requires an agent with a verified approval hook.
- `priority` (boolean) — Jump the queue
- `thread` (string) — latest (default), new, or a thread id
- `wake` (boolean) — Resume a paused workstation. false returns 409 instead of paying for a resume.

Responses:

- **202** — Accepted, with turn_id, journal cursor and stream URL
- **400** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — error, with a machine-readable `code` and a message written for a person
- **429** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person
- **503** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/turns/{turn}`

**Get one turn**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates.

Parameters:

- `turn` (path) *(required)*

Responses:

- **200** — The turn and how it ended
- **404** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `DELETE /v1/sandboxes/{id}/agent/turns/{turn}`

**Dequeue a turn that has not started**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates. A running turn must be interrupted instead; deleting it returns 409.

Parameters:

- `turn` (path) *(required)*

Responses:

- **200** — Removed
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/turns/{turn}/interrupt`

**Stop a running turn**

Only for sandboxes whose template declares kind=agent. Any other sandbox returns 404 — indistinguishable from a sandbox that does not exist, so the surface cannot be used to enumerate templates. Signals the agent's process group, honours a grace period, then kills it. Output produced before stopping is kept.

Parameters:

- `turn` (path) *(required)*

Body:

- `grace_seconds` (number) — How long the agent may stop politely before being killed

Responses:

- **200** — Interrupting
- **404** — error, with a machine-readable `code` and a message written for a person
- **409** — error, with a machine-readable `code` and a message written for a person
- **501** — error, with a machine-readable `code` and a message written for a person

## Webhooks

### `GET /v1/sandboxes/{id}/agent/webhooks`

**List webhooks for a sandbox**

Never returns signing secrets — they are shown once at creation. Also returns the signature scheme (what is signed, the skew window, the dedupe header) and the events this server will actually deliver, filtered through the predicate that enforces it.

Responses:

- **200** — OK
- **404** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/webhooks`

**Create a webhook**

The URL is SSRF-validated at creation as well as at send time, so a blocked target is an error you can read rather than a delivery that silently never arrives. Private, link-local, metadata, CGNAT and plaintext targets are refused.

`turn.output` is not deliverable: it is the agent's raw output stream, flushed several times a second, and forwarding it would flood your endpoint.

The signing secret is returned ONCE and never again.

Responses:

- **201** — Created; carries the signing secret once
- **400** — error, with a machine-readable `code` and a message written for a person
- **404** — error, with a machine-readable `code` and a message written for a person

### `GET /v1/sandboxes/{id}/agent/webhooks/failures`

**Deliveries that never got through (dead-letter view)**

Deliveries still failed after the attempt budget. A 4xx other than 408/429 stops immediately, because the receiver understood the request and refused it.

Responses:

- **200** — OK
- **404** — error, with a machine-readable `code` and a message written for a person

### `DELETE /v1/sandboxes/{id}/agent/webhooks/{hookID}`

**Delete a webhook**

Parameters:

- `hookID` (path) *(required)*

Responses:

- **200** — Deleted
- **404** — error, with a machine-readable `code` and a message written for a person

### `POST /v1/sandboxes/{id}/agent/webhooks/{hookID}/test`

**Send one signed test delivery**

Rate limited to 5 per minute per hook: this makes the server call your endpoint on demand. A failure at your endpoint is reported as 200 with `ok: false` — the API call succeeded, and returning 5xx would send you to our status page for a problem in your service.

Parameters:

- `hookID` (path) *(required)*

Responses:

- **200** — The probe result, including a failure at your endpoint
- **404** — error, with a machine-readable `code` and a message written for a person
- **429** — error, with a machine-readable `code` and a message written for a person


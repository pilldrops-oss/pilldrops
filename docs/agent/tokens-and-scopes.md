# Agent access tokens: scopes, lifetimes, and what each one really lets someone do

Agent access tokens (`sfa_<id>_<secret>`) are how you let something *other than your own
dashboard* drive one agent workstation — a CI job, a script, a colleague you sent a link to.

They are not API keys. An API key is your whole account. One of these is **one sandbox, one set
of scopes, one expiry**, and the point of the design is that handing one out is a small,
reversible decision rather than a large irreversible one.

## The scopes

| Scope | Lets the holder | Does **not** let them |
|---|---|---|
| `agent:read` | Read status, turns, the event stream, the policy, the spend figure | Submit anything, decide anything |
| `agent:write` | Submit turns, interrupt, reorder and remove queued turns | Open a shell, decide approvals, change the spend cap |
| `agent:approve` | Allow or deny parked tool calls, and forget remembered grants | Submit turns |
| `agent:files` | Read and write files in the workspace | Run commands |
| `agent:console` | Open an interactive shell in the sandbox | — (see the ceiling below) |
| `agent:admin` | Change the spend cap, mint and revoke other tokens | — |

### No scope implies another

This is the single most important property, and it is deliberate rather than an oversight.

The obvious-looking shortcut is to make `write` imply `read` — a token that can submit work
surely ought to be able to watch it. We do not, because implication is how privilege quietly
accumulates: once `write ⊃ read` feels natural, `console ⊃ write` follows, and then a token
minted to "just run the tests" is a shell. Every scope you want, you name.

The pairing you almost always want for an integration is `agent:read` **and** `agent:write`. That
is exactly what the connect bundle mints.

### Why `agent:console` is separate from `agent:write`

A token with `write` can ask the agent to do things. The agent is still the thing deciding how,
it is still subject to the spend cap, and in Guided mode its tool calls are still reviewed.

A token with `console` is a shell. There is no agent in the loop, nothing is journaled as a turn,
the approval gate never sees it, and the spend cap is irrelevant because no model is being
called. It is a fundamentally different kind of access that happens to be reachable through the
same URL prefix, and rolling it into `write` would mean every CI credential in existence was
also an interactive shell into a machine holding a provider key.

So: `console` is never implied, never included in a bundle you did not explicitly ask for, and
carries a hard ceiling on its lifetime.

### Why `agent:approve` is separate too

An approval is a human saying yes. A token that can *submit* work and also *approve* it has
turned Guided mode into Autonomous mode with extra steps — the agent asks, and the same
automation that asked answers. Keeping them separate means "someone reviewed this" stays true.

If you genuinely want unattended action, choose Autonomous at launch. That is honest. Granting
`write + approve` to a bot is the same thing while looking like review.

## Lifetimes

| Token shape | Default lifetime | Ceiling |
|---|---|---|
| Read-only (exactly `agent:read`) | 24 hours | — |
| Anything that can act | 7 days | — |
| Anything carrying `agent:console` | **15 minutes** | **15 minutes, enforced** |
| Connect bundle | 24 hours | — |

The console ceiling is a maximum, not a default you can raise: asking for longer is refused
rather than clamped silently, because a caller who asked for 24 hours and got 15 minutes without
being told will build something that breaks at an unpredictable time.

Read-only defaults to a day because that is overwhelmingly what read-only tokens are — a link
handed to someone to watch a run. A forwarded link then goes stale on its own, which is the
behaviour you want from something that gets pasted into a group chat.

## Rotation and revocation

- `POST …/agent/tokens/{tokenID}/rotate` issues a new secret for the same token id and scopes.
  The old secret stops working immediately. There is no overlap window — an overlap is a second
  live credential, and the reason you are rotating is usually that you do not know where the
  first one went.
- `DELETE …/agent/tokens/{tokenID}` revokes. Immediate, no propagation delay: the check is a
  lookup on every request rather than a cached decision.
- The secret is shown **once**, at mint. We store a SHA-256 hash, so we cannot show it again —
  and neither can anyone who reads the database.

## Two things worth knowing before you paste a token anywhere

**`X-Agent-Token` takes precedence and never falls back.** If you send that header with something
that does not look like an agent token, the request is refused — it does not quietly try your
`Authorization` header instead. A request arriving through a proxy hop often carries an operator
key in `Authorization`, so falling back would *widen* a request that was meant to be narrow. Be
explicit about which credential you are using.

**Share links land on a separate origin.** A link goes to `agent-<sandbox>.<domain>`, not to the
dashboard, so the page it opens cannot read your dashboard session or act as you. That is also
why the viewer there is a small read-only page rather than the full app.

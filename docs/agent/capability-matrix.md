# Agent capability matrix

> **Generated file — do not edit.** Rendered from the launch manifest (internal/templates) by
> `cmd/gendocs`. Run `make docs` after changing the source; a test fails if this
> file is stale. Editing it by hand means the next regenerate silently discards
> your words.

Every column is a capability that was **verified against a live CLI** at the pinned version in
the manifest, not read from a vendor's documentation. That distinction is the point of the
table: several of these agents advertise features their CLI did not actually deliver when we
ran it, and a platform that repeated the claim would be lying on their behalf.

A blank cell is therefore not a gap in our research — it is a capability the agent did not
demonstrate, and the consequences of each are spelled out below the table.

| Agent | Follow-up | Approval gate | Tool timeline | Model choice |
|---|---|---|---|---|
| `amp` | — | — | — | — |
| `claude` | yes | yes | yes | yes |
| `codex` | yes | — | yes | — |
| `grok` | — | — | — | — |
| `hermes` | yes | — | — | — |
| `openclaw-agent` | yes | — | — | — |
| `opencode` | yes | — | yes | — |

### What a blank cell costs you

**Follow-up** — the agent's CLI has no resume flag we could verify, so every message starts a
fresh conversation. It will not recall what you said a minute ago. The status endpoint reports
this as `remembers: false` with a note, so a client can say so rather than presenting a
thread that silently forgets.

**Approval gate** — the CLI has no hook we could route tool decisions through, so **Guided mode
is not offered** for this agent. It is not disabled out of caution: an agent that accepted the
flag and ignored it would run completely ungated while the interface claimed every action was
reviewed, which is worse than having no approvals feature at all. Supervised and Autonomous
still work.

**Tool timeline** — no structured event stream, so the transcript is the agent's raw output
rather than a list of tool calls with durations. Everything still runs; you just see it as text.

**Model choice** — the agent takes its model from its own config file rather than an environment
variable, so the Model tab writes that file instead of setting a variable.

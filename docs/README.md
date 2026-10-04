# Documentation

The [README](../README.md) covers what PillDrops does and a quick start. These
go further. [`architecture.md`](architecture.md) explains how it is built: the
tiers and their credentials, a request end to end, how a paused sandbox
outlives its node, and the repository layout.

## Using it

| | |
|---|---|
| [`pilldrops-cli.md`](pilldrops-cli.md) | The CLI's commands. |
| [`template-registry.md`](template-registry.md) | What a template is and how to publish one. |
| [`snapshots.md`](snapshots.md) · [`checkpoints.md`](checkpoints.md) · [`fork.md`](fork.md) | The three ways to capture and reuse a sandbox's state. |
| [`runtimes.md`](runtimes.md) | Firecracker, Docker and the in-process dev runtime, and what each isolates. |

## Running it

| | |
|---|---|
| [`deployment.md`](deployment.md) | The operator runbook: every setting, TLS, joining nodes, object storage, node loss, upgrading Firecracker. |
| [`releases.md`](releases.md) | How a `sandboxd` release is signed, and how to verify one. |

The server also serves an interactive API reference at `/docs` and the raw
OpenAPI document at `/openapi.json`.

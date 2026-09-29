# Contributing to PillDrops

Thanks for your interest in contributing! PillDrops is an open-source platform
for running untrusted code in isolated environments (Firecracker microVMs and
containers), so correctness and isolation are held to a high bar. This guide
covers how to get set up and land a change.

## Getting oriented

New to the codebase? The README's *Architecture* and *Repository layout*
sections cover the tiers, a request end to end and where each part lives;
[`docs/`](docs/README.md) has the runbook and the feature docs.

## Prerequisites

- **Go 1.25+** (see `go.mod` for the exact version)
- `make`, a C compiler (the race detector in `make test` uses cgo), a POSIX
  shell, and Docker (for the container runtime and local DB)
- Node.js and pnpm if you change the dashboard in `web/` (`make web` rebuilds
  the bundle embedded in `internal/webui/dist`, which must be committed)
- Linux with KVM to exercise the Firecracker runtime. On macOS only `make dev`
  runs sandboxes, and it runs them unisolated as your user

## Build, run, and test

```bash
make build        # build the node agent, envd, and agentd
make dev          # run the dev server on http://127.0.0.1:8080 (loopback, anonymous)
make test         # run the Go test suite
make test-cover   # tests with a coverage report
make check        # everything CI runs: builds, Linux tests, guards, lint, scanners
```

`make dev` binds to loopback and runs with anonymous access for local
development only. Never set `PORTAL_ALLOW_PUBLIC_ANON=1` on a shared or
cloud host.

## Linting

CI runs `golangci-lint` with the config in `.golangci.yml`. Please run it
locally before opening a PR:

```bash
golangci-lint run ./...
go vet ./...
```

## Making a change

1. Fork the repository and create a topic branch off `main`.
2. Keep changes focused; prefer small, reviewable commits with clear messages.
3. Add or update tests for behavior you change. Isolation-, auth-, and
   lifecycle-related changes should come with tests that would fail without the
   fix.
4. Run `make check` (on Linux, or with a Linux host) — it must pass.
5. Open a pull request describing the motivation, the approach, and how you
   verified it. Link any related issue.

## Commit and PR conventions

- Write imperative, present-tense commit subjects (e.g. "add pids-limit to
  container runtime"), and explain the *why* in the body when it isn't obvious.
- One logical change per PR where practical.

## Reporting security issues

Please do **not** open a public issue for a security vulnerability. See
[`SECURITY.md`](SECURITY.md) for private disclosure instructions.

## License of contributions

By contributing, you agree that your contributions are licensed under the
Apache License 2.0 (the project's license — see [`LICENSE`](LICENSE)). Inbound
contributions are under the same terms as the outbound project license.

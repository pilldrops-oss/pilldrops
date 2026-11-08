# PillDrops

**Open-source, self-hosted sandboxes for AI agents and untrusted code.**

PillDrops gives every piece of code its own disposable Linux machine: a
Firecracker microVM with its own kernel, created in one API call. Run
AI-generated code, host an app on a preview URL, give a coding agent a real
computer, then pause it, copy it, roll it back or throw it away. It runs on
your own hardware, from one laptop to a fleet.

```python
from pilldrops import Sandbox

with Sandbox.create(template="python-3.11") as sb:
    print(sb.run("python3 -c 'print(6 * 7)'").stdout)   # 42
```

> ⚠️ PillDrops runs untrusted code. Read [`SECURITY.md`](SECURITY.md) before
> exposing a server, and never expose the local development server.

## What you can do with it

- **Run untrusted code safely.** Each sandbox is a microVM with its own kernel,
  network namespace and resource limits, isolated from the host and from
  other sandboxes.
- **Run commands, code and files.** Execute shell commands or code snippets,
  read and write files, upload whole directories, open an interactive terminal.
- **Host apps on a URL.** Start a server in a sandbox and get a preview URL for
  any port, private by default or shareable.
- **Pause and resume.** Freeze a sandbox with its memory and running processes,
  and wake it later exactly where it was. Idle sandboxes pause themselves.
- **Fork.** Copy a running sandbox, processes included, in milliseconds, and
  try several things in parallel.
- **Checkpoints.** Save a sandbox and roll it back to that point in place.
- **Snapshots.** Set a machine up once, save it, and start new sandboxes from it
  with everything already installed.
- **Volumes.** Keep data on a disk that outlives the sandbox and attach it to
  the next one.
- **Control the network.** Allow or deny outbound traffic by domain or address,
  per sandbox.
- **Bring your own image.** Use a template (Python, Node, Go, browsers,
  desktops, coding agents…) or any Docker image.
- **Scale to a fleet.** Add nodes behind a control plane; sandboxes are
  scheduled across them, can move between them, and paused ones survive losing
  their node.
- **Drive it from anywhere.** Python and JavaScript SDKs, a CLI, an HTTP API, a
  dashboard, and an MCP server for AI agents.

## Quick start

### 1. Run a PillDrops server

To try the API and dashboard on your own machine (macOS or Linux, needs Go):

```bash
git clone https://github.com/pilldrops-oss/pilldrops && cd pilldrops
make dev          # http://localhost:8080 — dashboard at /dashboard, API docs at /docs
```

This development server runs sandbox commands **as your user, without
isolation**, and only listens on localhost. For real isolation, run a node on
Linux with KVM (Firecracker) or Docker:

```bash
make sandboxd
export SANDBOXD_DATA=/var/lib/sandboxd
sudo -E ./bin/sandboxd                        # serves on :8080
sudo cat /var/lib/sandboxd/bootstrap.key      # your API key, for SDKs and the CLI
```

Open `http://<server>:8080/dashboard`: the first visit asks you to create the
admin account (email and password), which signs you in from then on.

`scripts/provision-node.sh` sets up a bare KVM host (Firecracker, kernel,
templates). See [deployment](docs/deployment.md) for everything else.

### 2. Install an SDK and point it at your server

```bash
pip install pilldrops              # Python
npm install @pilldrops/sdk         # JavaScript / TypeScript
npm install -g @pilldrops/cli      # CLI

export PILLDROPS_API_URL=http://localhost:8080
export PILLDROPS_API_KEY=<your key>          # not needed for make dev
```

### 3. Run code in a sandbox

**Python**

```python
from pilldrops import Sandbox

with Sandbox.create(template="python-3.11") as sb:
    sb.files.write("hello.py", "print('hello from a sandbox')")
    print(sb.run("python3 hello.py").stdout)
```

**JavaScript / TypeScript**

```ts
import { Sandbox } from '@pilldrops/sdk'

const sb = await Sandbox.create({ template: 'python-3.11' })
const result = await sb.run("python3 -c 'print(6 * 7)'")
console.log(result.stdout)   // 42
await sb.kill()
```

**CLI**

```bash
pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- python3 -c 'print(6 * 7)'
pilldrops connect <id>                      # an interactive shell
```

## More things to try

**Host an app and get its URL**

```python
app = Sandbox.create(template="python-3.11",
                     start="python3 -m http.server 8000", ports=[8000])
print(app.url)
```

**Pause and resume, with memory intact**

```python
sb.pause()      # memory written to disk, RAM freed
sb.resume()     # same processes, same files
```

**Fork, checkpoint and snapshot**

```bash
pilldrops sandbox fork <id>                              # a running copy
pilldrops checkpoint save <id> before-upgrade            # a save point...
pilldrops checkpoint restore <id> before-upgrade --confirm   # ...and back to it
pilldrops snapshot save <id> my-env                      # a reusable image
pilldrops sandbox create --from-snapshot my-env          # start from it
```

**Allow only the network it needs**

```bash
curl -X POST $PILLDROPS_API_URL/v1/sandboxes \
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \
  -d '{"template": "python-3.11", "network": {"allow": ["pypi.org", "*.pythonhosted.org"]}}'
```

**Give an AI agent sandboxes as tools (MCP)**

```bash
claude mcp add pilldrops -- pilldrops mcp       # via the CLI, any server
# a control plane also serves MCP over HTTP at $PILLDROPS_API_URL/mcp
```

More in the [examples](https://github.com/pilldrops-oss/pilldrops-examples):
a code interpreter, browser automation, running untrusted code, a coding agent,
app previews and evals at scale.

## What people use it for

- **AI agents that write and run code**, with a machine they cannot escape,
  paused between turns and forked to try several approaches.
- **Code execution in your product**: notebooks, playgrounds, graders,
  interview environments.
- **Throwaway environments**: preview a branch, run end-to-end tests in a real
  browser, then delete the machine.
- **Long-lived workspaces** that sleep when idle and wake in seconds.

## How it works

A **node** (`sandboxd`) runs sandboxes as Firecracker microVMs, or Docker
containers where KVM is not available; a small agent inside each sandbox runs
commands and moves files. One node is a complete server. For more, a **control
plane** (`orchestrator`) schedules sandboxes across nodes, routes each request
to the right one, and with an S3-compatible bucket keeps paused sandboxes
recoverable when a node is lost. The [architecture](docs/architecture.md) page
has the details.

## Documentation

- [Deployment](docs/deployment.md): running a node or a fleet, every setting,
  TLS, object storage, upgrades
- [Architecture](docs/architecture.md), and [runtimes](docs/runtimes.md) with
  what each one isolates
- [Checkpoints](docs/checkpoints.md), [fork](docs/fork.md) and
  [snapshots](docs/snapshots.md)
- [CLI reference](docs/pilldrops-cli.md)
- The full HTTP API reference is served by every server at `/docs`

SDKs: [Python](https://github.com/pilldrops-oss/pilldrops-python) ·
[JavaScript](https://github.com/pilldrops-oss/pilldrops-js) ·
[CLI](https://github.com/pilldrops-oss/pilldrops-cli)

## Development

```bash
make build     # sandboxd and envd for linux/amd64
make test      # Go tests (needs a C compiler for the race detector)
make check     # everything CI runs; run it on Linux before opening a PR
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Report vulnerabilities privately as
described in [`SECURITY.md`](SECURITY.md).

## License

[Apache License 2.0](LICENSE).

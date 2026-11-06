# PillDrops CLI

Single-binary customer CLI, from the
[pilldrops-cli](https://github.com/pilldrops-oss/pilldrops-cli) repo:
`npm install -g @pilldrops/cli`, or `go build ./cmd/pilldrops` there.

## Sign in

There is one credential: the API key. On a fresh node it is written to
`$SANDBOXD_DATA/bootstrap.key` on first start (see the README's self-hosting
section); after that you mint more from the dashboard.

    export PILLDROPS_API_URL=https://<your-server>
    export PILLDROPS_API_KEY=sfc_...

or store it in `~/.pilldrops/config.json` (0600):

    pilldrops login --api-url https://<your-server> --key sfc_...

The browser sign-in flow that used to exist here needed a hosted identity
provider, which the open-source core deliberately does not have.

## Who can see a sandbox

The API key is the member. Every sandbox records the key that created it
(`created_by`) and a visibility:

- **org** — the default, and what every sandbox was before visibility existed:
  every key in the org lists it, execs in it and opens its terminal.
- **private** — only the creating key and keys with the operator role. To
  everyone else in the org it does not exist: 404, and absent from `ls`.

    pilldrops sandbox create --template base --private   # only your key sees it
    pilldrops sandbox share <id>     # private -> org, and the agent logins inside are removed
    pilldrops sandbox unshare <id>   # org -> private; nothing is restored
    pilldrops sandbox fork <id> [--shared]   # the copy is yours and private unless --shared

**Sharing wipes the agent logins.** Handing a private sandbox to the org
hands over whoever is signed in inside it, so `share` removes
`~/.claude/.credentials.json`, `~/.codex/auth.json` and
`~/.local/share/opencode/auth.json`, and drops the per-sandbox env the
dashboard stored on it, before anyone else can reach the machine. The response
lists what it removed. A paused sandbox is woken to do it.

**Unshare restores nothing.** There is nowhere to restore from: the files are
deleted, not stashed, and no copy of anyone's OAuth token is kept in the
control plane. After an unshare, log in again inside the sandbox
(`claude`, `codex`, `opencode`). This is deliberate, not a gap waiting to be
filled — the alternative is a server-side store of personal refresh tokens.

**A fork is private to whoever forked it**, even a fork of a sandbox the whole
org can see: the copy carries the parent's files and running processes, which
belong to the person who made them. Pass `--shared` to hand the copy to the org.

### Secrets follow the same rule

A vault secret carries a scope, `all` by default, which is how every existing
secret behaves:

    pilldrops env scope OPENAI_KEY private    # only my own private sandboxes
    pilldrops env scope DEPLOY_KEY shared     # only sandboxes the org can see
    pilldrops env list --scope private

A `private` secret is injected only into a private sandbox created by the key
that stored it; a `shared` secret only into an org-visible one. A create that
names a secret its scope forbids is refused, naming the secret — it is not
dropped silently, because a sandbox missing a credential it asked for fails
later and further away. A scoped secret also stops auto-injecting: "every
sandbox this org creates" cannot honour a scope.

Changing a scope never touches the value.

Two neighbouring things are *not* this:

- `--public` at create governs the **preview URL** only: whether the app the
  sandbox serves is reachable without a preview token. It says nothing about
  who may reach the API.
- Billing is per org either way; visibility changes reachability, not who pays.

## What this server supports

The CLI works with any PillDrops server. It reads `features` from
`GET /v1/capabilities`, and help, `pilldrops ai` and `pilldrops mcp` then show
only what that server has. This server reports none of the optional groups
(agent, chat, cron, function, integrations, team, thread, volume), which are on
PillDrops Cloud. Running one exits 1 with the code `feature_unavailable` before
anything is sent. Logged out, or against an older server that does not report
`features`, the CLI shows everything.

## Commands

    pilldrops templates
    pilldrops sandbox create --template python-3.11 [--vcpu 2 --mem 1024]
    pilldrops sandbox create --template base --slug myapp   # https://myapp.<domain>/ ; every <id> below also accepts a slug
    pilldrops sandbox ls | get <id> | rm <id> | pause <id> | resume <id>
    pilldrops sandbox ports <id>                  # ports with a listener (probes a fixed candidate list)
    pilldrops sandbox expose <id> 5432 [--tcp] [--udp]   # publish a guest port on a raw public endpoint
    pilldrops sandbox forwards <id> | unexpose <id> 5432
    pilldrops sandbox set-port <id> 3000          # move the preview to another port, no restart
    pilldrops sandbox rename <id> [--name <label>] [--slug <slug>]   # empty string clears either
    pilldrops sandbox create --template base --networks web,db        # join opt-in networks at create
    pilldrops sandbox networks <id> web,db | --clear                  # replace or drop them on a running sandbox
    pilldrops sandbox fork <id> [--name <label>] [--timeout <s>] [--json]   # running copy, memory and processes included (docs/fork.md)
    pilldrops sandbox checkpoint save|list|rm|restore <id> [<name>] [-y] [--json]   # named save points, rolled back in place (docs/checkpoints.md)
    pilldrops snapshot save <id> <name> [--wait] [--json]   # publish an org-owned golden image (docs/snapshots.md)
    pilldrops snapshot list [--json]                        # every name and version, newest first
    pilldrops snapshot rm <name> [-y]                       # delete every version and its objects
    pilldrops sandbox create --from-snapshot <name>         # wake a NEW sandbox inside that capture
    pilldrops snap ...                                      # `snap` is an alias for `snapshot`
    pilldrops exec <id> -- python -c 'print(1)'    # exits with the remote code
    pilldrops logs <id> [-f]
    pilldrops cp app.py <id>:/app/app.py
    pilldrops cp <id>:/app/out.txt ./out.txt
    pilldrops cp -r ./proj <id>:.          # lands at ~/proj (scp semantics)
    printf 'data' | pilldrops cp - <id>:x  # "-" is stdin (upload) or stdout (download)
    printf '{"a":1}' | pilldrops exec <id> -- jq .a   # a pipe becomes the command's stdin
    pilldrops env set NAME VALUE [--secret] [--auto] | env list | env rm NAME
    pilldrops domain add app.example.com --sandbox <id> [--port 3000] [--confirm]
    pilldrops domain add '*.vms.example.com'       # one host per sandbox, by label
    pilldrops domain list | domain rm app.example.com
    pilldrops ps <id>
    pilldrops metrics <id>
    pilldrops usage
    pilldrops whoami | logout

## Raw port forwarding

`expose` publishes a guest port on `<forwarding host>:<public port>` and prints
the endpoint. The bytes are untouched in both directions, so a Postgres client
or an SSH client connects straight to it — no HTTP in the path. Naming neither
`--tcp` nor `--udp` means TCP. Re-exposing the same guest port keeps its public
port and only changes the protocol set, so a published endpoint stays valid.

A connection to a paused sandbox wakes it; the first connection after a wake
can take ~20 s while the sandbox restores and its app binds. The forward dies
with the sandbox: destroying it frees the public port and closes open streams.

`--udp` needs a Firecracker sandbox: Docker publishes container ports as TCP
only, so a UDP forward there is refused with `udp_not_supported`.

Plans cap the number of distinct guest ports per sandbox (three by default);
the fourth is refused with the plan and the count. A Docker-runtime sandbox can
only forward ports declared when it was created — anything else is refused with
`port_not_declared` rather than accepted as an endpoint that cannot connect.

## Custom domains

`domain add` prints the one DNS record to create and then waits: nothing routes
until the platform has resolved that record itself, so the command exits
non-zero (with the resolver's reason) if the record never appears. `--confirm`
skips the "press Enter when the record exists" pause for scripts.

    $ pilldrops domain add app.example.com --sandbox sb-7f3
    Create this DNS record:

        app.example.com CNAME 8000-sb-7f3.sb.example.com

    Press Enter once the record exists (Ctrl-C to check later with `pilldrops domain list`).
    app.example.com is live

- The sandbox must be **public** (`--public` at create, or the dashboard
  toggle). A private preview answers 403 without `?token=`, so a customer host
  in front of one would refuse every visitor; the API refuses with
  `sandbox_not_public` instead of attaching it.
- `--port` follows the same rule as the preview URL: any port on Firecracker,
  the published port only on a single-port (docker) runtime.
- A wildcard row (`*.vms.example.com`, no `--sandbox`) serves every sandbox of
  your org at `<port>-<id>.vms.example.com` or `<slug>.vms.example.com`. The
  bare domain never routes.
- HTTPS is the deployment's terminator, which asks the platform before issuing
  a certificate — a host that has not verified never gets one (docs/deployment.md).

## Sandbox networks

Sandboxes cannot reach each other. That is the default, it is measured
(`scripts/security-suite.sh`, `block.peer.*`), and nothing here changes it for a
sandbox that does not ask.

A sandbox may name up to eight **network tags** (`^[a-z0-9][a-z0-9-]{0,31}$`).
Two sandboxes reach each other, on any port, **if and only if** they are in the
same org and share at least one tag. Tags are per org: two orgs picking the same
word get two different networks.

    pilldrops sandbox create --template base --slug api --networks backend
    pilldrops sandbox create --template base --slug worker --networks backend
    pilldrops exec worker -- curl http://api.pd:8000/health

`pilldrops sandbox get <id>` shows `networks`, `private_ip` (the address peers
reach it on) and `node`. A peer is resolvable as `<slug>.pd` from inside a
sandbox that shares one of its tags, TTL 30 s; any other `.pd` name is NXDOMAIN
and every other name resolves as before. `pilldrops sandbox networks <id> a,b`
replaces the membership on a running sandbox — it takes effect in well under a
second with no restart — and `--clear` closes it off again.

**Same node only, and this is not a soft limit.** Reachability is built out of
per-node addresses, so two sandboxes on different nodes do not connect and
`<slug>.pd` does not resolve, even with the same tag. `get` shows `node` for
exactly this reason: if two sandboxes that should see each other cannot, compare
those two values first. Pin placement with `--runtime`/`--zone` if you need them
co-located; there is no overlay.

The node's runtime has to support it. Firecracker with per-sandbox namespaces
(`FC_NETNS=1`) does; anything else answers **501** `tag_networks_unsupported`
naming the runtime rather than handing back a sandbox that is on no network.
`GET /v1/capabilities` reports `tag_networks`.

## Limits

Every number here is the default of the constant or environment variable it
names, so a self-hoster can raise it knowingly.

| limit | value | where |
|---|---|---|
| per-file `/v1/sandboxes/{id}/files` upload | 1 GiB | `PD_FILES_MAX_BYTES` (node); a larger file is a 413 naming it |
| other request bodies | 64 MiB | `MAX_REQUEST_BYTES`; `/files` is exempt and uses the row above |
| exec output | 10 MiB, then the command is killed | `maxOutput` in envd; `POST /exec/stream` with `raw: true` has no cap and returns stdout bytes with the exit code in `X-Exit-Code` |
| exec stdin | 64 MiB | `maxExecStdinBytes` (node), sent base64 in the `stdin` field |

## Idle timers, and the footgun in them

A sandbox has two idle timers, both measured from the last time the **platform**
saw activity — an API call, an exec, a preview request. Neither watches what the
guest is doing.

| timer | what it does | default |
|---|---|---|
| `idle_timeout` | **standby**: freezes the sandbox in RAM. Processes, memory and open sockets are kept; a wake is milliseconds and nothing restarts. | off |
| `hibernate_timeout` | **pause**: writes the memory to disk and frees it. A wake restores and relaunches the start command. | 4 h |

Both accept seconds, clamp to 30 s..24 h, and take `-1` for never and `0` for
the platform default. Set either at create (`--idle-timeout`,
`--hibernate-timeout`) or on a live sandbox (`sandbox set-idle`,
`sandbox set-hibernate`, or `PATCH /v1/sandboxes/{id}`).

**The footgun: a long job with no API traffic looks idle.** A training run, a
build or a batch job that runs for an hour without anyone calling the API is
indistinguishable from an abandoned sandbox, and the timers will freeze it and
then write it to disk. Standby is harmless here — the job resumes exactly where
it was — but a hibernate stops the processes. Two escapes:

- `pilldrops sandbox set-idle <id> -1` and `set-hibernate <id> -1`, or
  `idle_timeout: -1` / `hibernate_timeout: -1` at create: never auto-tier.
- `POST /v1/sandboxes/{id}/refresh` from the job itself, on a heartbeat: it
  re-stamps the activity clock, so a job that checks in every few minutes is
  never seen as idle.

**What each tier does to the guest's clock.** A frozen sandbox is not a slowed
one: its vCPUs stop and its clocks keep running. A thawed guest therefore sees
time jump forward by the time it was frozen, and anything it had armed — a
`sleep`, a timeout, a retry — fires at the thaw, exactly as on a laptop
resuming from suspend. Measured: a timer armed for 30 s and then frozen for
60 s fires when the sandbox thaws, not 60 s after it. Hibernate is the
opposite — the guest's clocks stop with it — so the node steps the guest's wall
clock forward when it restores, and the time on disk is missing from the
guest's monotonic clock rather than jumped over.

Paths. A relative or `~/` path is the sandbox home on both runtimes:
`/root` on Firecracker, `/workspace` on Docker. The Firecracker file API is
confined to that home — use `exec` to reach `/etc` or anywhere else outside
it; Docker also accepts absolute paths. `cp -r` treats the destination as the
**parent**, so `cp -r ./proj <id>:.` produces `~/proj`, like `scp -r`. The CLI
holds the file in memory while copying.

## Agent contract

- `--json` on any command → exactly one JSON value on stdout, nothing else.
- Errors: `{"error":..., "code":...}` on stderr in `--json` mode; plain text
  otherwise. The key is never printed.
- Exit codes: `0` ok · `1` API/network error · `2` usage error · `3` not
  authenticated (run `pilldrops login`). Exception: `exec` propagates the
  remote command's exit code (like `ssh`), so a remote exit of 2 or 3 is a
  real remote result, not a CLI usage/auth error.
- No prompts when stdin is not a TTY.

## Credential resolution order

`PILLDROPS_API_KEY` (env) beats the config file for the key; `--api-url` flag
beats `PILLDROPS_API_URL` (env) beats the config file for the URL.

## Inside a sandbox

Every `base` template ships `/usr/local/bin/pilldrops` (baked by
`internal/templatebuild` / `scripts/build-template.sh` from `make cli-linux`;
the docker runtime bind-mounts the same binary), so `pilldrops version` works
in any fresh sandbox.

Every sandbox also holds its **own identity**, delivered by the platform after
boot (over the vsock credential channel on Firecracker, by `docker cp` on
Docker — never on a kernel command line, never in `docker inspect`):

    /run/pilldrops/token     pd_sb_…  (0600, the sandbox's bearer token)
    /run/pilldrops/env       PILLDROPS_API_URL, PD_SANDBOX_ID, PD_SANDBOX_DOMAIN

The env file is layered into every exec, terminal and login shell, so from
inside a sandbox:

    curl -H "Authorization: Bearer $(cat /run/pilldrops/token)" \
         $PILLDROPS_API_URL/v1/sandboxes/$PD_SANDBOX_ID        # itself
    curl … $PILLDROPS_API_URL/v1/whoami    # {"principal":"sandbox","sandbox_id":…,"org":…}

The preview base for a port is `https://<port>-$PD_SANDBOX_ID.$PD_SANDBOX_DOMAIN`.

What the token can do — **its own sandbox** (exec, cp, ports, pause, destroy,
…) and, within the owning org, **create, list, get, exec, cp, fork** other
sandboxes; creates pass the org's admission and are billed to the org. What it
cannot do — keys, vault, billing, projects, images, admin, or destroy / pause /
reconfigure a sibling: those answer `403 principal is a sandbox`. Any route
not on that list is refused. Actions taken with it are audited as
`key_id = sb:<sandbox id>`.

Blast radius of a leaked token: this sandbox plus org-wide sandbox
create/exec/cp/fork, **until destroy** — the hash lives only on the sandbox's
own row, so destroy revokes it on the next request on every replica, and a
fork, restore or template built from the sandbox gets a fresh token pushed
over the channel (the stale one inside the image hashes to nothing). Nodes
hand out an identity only when they know the API URL (`PILLDROPS_API_URL`,
default `ORCHESTRATOR_URL`).

The CLI resolves `--api-url` flag → `PILLDROPS_*` env → `SANDFORGE_*` env →
`/run/pilldrops/{token,env}` → config file, so inside a sandbox it works with
no credential handed to it, and `pilldrops whoami` answers
`principal: sandbox`.

Local devserver: guests are firewalled from the node. With
`PILLDROPS_LOCAL_API_PORT=<port>` on a Firecracker node, exactly that port at
the guest's host veth address is opened and `PILLDROPS_API_URL` points there.

## Portal configuration (operator)

- `PORTAL_API_KEYS_FILE` persists the hash→owner bindings across restarts. It
  holds sha256 hashes only, never raw keys, but does contain owner IDs — it is
  gitignored.
- Keys are minted by `POST /v1/keys` (`pilldrops keys create <name>`), which
  takes one field, `name`: no scopes and no expiry. There is no browser login
  flow — there is no identity provider — and the store that a
  `/cli/auth` flow would have written has been deleted.

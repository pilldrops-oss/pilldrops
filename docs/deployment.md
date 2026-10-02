# Deploying a fleet

What a real multi-node install needs: DNS, TLS, the environment on each tier, and
the host setup a node requires before it can isolate anything.

Everything here is read from the code that consumes it, not from memory. Where a
setting has a trap, the trap is stated rather than the happy path.

**Upgrading a fleet that already exists?** Run `sudo scripts/pre-upgrade-check.sh`
on each node first — it exits 2 for a check it could not run, which is not a pass.
Five recent hardening changes are breaking for an existing install and silent for
a fresh one; see [SECURITY.md](../SECURITY.md#upgrading-an-existing-fleet).

---

## The shape

```
                    browser
                       │
        ┌──────────────┴───────────────┐
        │                              │
   <domain>                    *.<domain>
   dashboard + /v1           previews + agent origins
        │                              │
        └──────────────┬───────────────┘
                       │
                 orchestrator            (control plane: schedules, proxies,
                       │                  serves the dashboard, owns the store)
        ┌──────────────┼──────────────┐
      node           node           node  (sandboxd-fleet)
        │
    sandboxes
```

One address serves the dashboard, the API and every preview. Nodes are **not**
public — see *Nodes must not be reachable from the internet* below.

---

## DNS

Two records, and the wildcard is not optional if you want previews.

| record | points at | why |
|---|---|---|
| `<domain>` | orchestrator | dashboard, `/v1`, `/mcp` |
| `*.<domain>` | orchestrator | `<port>-<id>.<domain>` app previews and `agent-<id>.<domain>` |

**The wildcard is a security boundary, not a convenience.** A sandbox served on
the dashboard's own origin can, from an iframe with `allow-same-origin`, reach
`window.parent` and read the dashboard. The SPA knows this and **fails closed** —
`isPreviewCrossOrigin()` returns false and it refuses to embed — so a deployment
without the wildcard does not degrade quietly, it visibly loses the preview.

## TLS

A **wildcard certificate** for `*.<domain>`, terminated in front of the
orchestrator. Every sandbox gets its own hostname, so a per-host certificate is
not workable.

If TLS terminates at a proxy (it usually does), that proxy must set
`X-Forwarded-Proto`. The control plane reads it to decide whether the preview
cookie gets `Secure`. Getting this wrong is not catastrophic — forging it
downward only drops `Secure` from the forger's own cookie — but a proxy that
never sets it leaves every preview cookie non-Secure.

Set `TRUSTED_PROXY_CIDRS` to the terminator's own addresses. Without it the
control plane ignores `X-Forwarded-For` entirely and judges every request by
the TCP peer — which is the terminator, so per-IP rate limits and the per-org
preview source gate would see one client for the whole internet.

### Customer domains (`myapp.customer.com`)

A customer hostname is a row (`POST /v1/domains`), verified by DNS, and served
by the same preview path as the platform wildcard. The certificate is the
deployment's half of the job: put Caddy in front of the orchestrator with
on-demand TLS and let it ask the platform which hosts are real.

```caddyfile
{
  on_demand_tls {
    # 200 = this host is verified and routes; 404 = do not ask a CA for it.
    ask http://orchestrator.internal:9090/v1/domains/check
  }
}

# The platform's own wildcard: one certificate, DNS-01 (Caddy cannot do
# HTTP-01 for a wildcard), or drop in a certificate you already have.
*.example.com, example.com {
  tls { dns <your-provider> {env.DNS_API_TOKEN} }
  reverse_proxy orchestrator.internal:9090
}

# Every other hostname: a certificate is issued on the first handshake, but
# only for a host `ask` approves.
:443 {
  tls { on_demand }
  reverse_proxy orchestrator.internal:9090
}
```

- `/v1/domains/check` is unauthenticated on purpose — it answers during a TLS
  handshake, before any credential exists — and it answers **200 only for a
  host whose DNS record the verifier has already seen**. Let's Encrypt
  rate-limits per registered domain, so an `ask` that approved anything would
  let a stranger burn the quota for every customer.
- :80 must be reachable: on-demand issuance uses HTTP-01.
- Ceiling: ~50 certificates per registered domain per week. An org wildcard
  (`*.vms.customer.com`) issues one certificate per sandbox hostname, so a
  customer creating many named sandboxes a week will hit it. The fix, when
  someone does, is NS delegation with DNS-01 — see the change's design.md.
- A customer who would rather keep TLS themselves can terminate it on their own
  proxy and point it at the platform host over plain HTTP; the row still has to
  verify before anything routes.

## Raw port forwarding

`POST /v1/sandboxes/{id}/forwards {"port": 5432}` publishes a guest port on
`<FORWARD_PUBLIC_HOST>:<public port>` and splices raw TCP to it — no HTTP, no
interpretation, so a database client or an SSH server works. The row is the
whole feature: it survives a control-plane restart and a node restart, and
destroying the sandbox frees the public port.

**Exactly one replica may run the ingress.** A raw TCP port terminates on one
host, and the HTTP load balancer in front of the replicas does not carry
arbitrary ports at all. Start one replica with `FORWARD_INGRESS=1`; it binds
every active row, reconciles on every add and delete it serves and on a 30 s
tick, and it is the host `FORWARD_PUBLIC_HOST` must resolve to. Every other
replica serves the forwards API normally — the row is the truth and the ingress
picks it up on the next tick. Set `FORWARD_INGRESS=remote` on those API-only
replicas: it is how they know an ingress exists at all. With the variable unset
everywhere, `POST /forwards` is refused with 501 `no_forward_ingress` rather
than answering 201 with an endpoint nothing will ever bind.

**Firewall:** open `FORWARD_PORT_RANGE` (default `40000-60000`, TCP and UDP) on
the ingress host only. Nodes stay private: the ingress reaches the guest over
the existing pinned orchestrator→node hop, not by exposing a node port.

**The ceiling is that one host** — its bandwidth, and its 20 000 ports. Against
a fleet ceiling of around 95 sandboxes at three forwards each that is not
close. The upgrade path is **not** an L4 load balancer: add an `ingress_id`
column, let several `FORWARD_INGRESS` replicas claim rows, and return that
replica's own `FORWARD_PUBLIC_HOST` from the API. Each forward still terminates
on exactly one host; only the allocator learns a second key.

**UDP needs a Firecracker node.** Docker publishes a container port as TCP only
(`-p 127.0.0.1:0:<port>`), so a UDP forward there is refused with 409
`udp_not_supported` rather than bound to a socket nothing can answer. A UDP
forward carries datagram boundaries and the client address end to end; a burst
faster than the guest drains is dropped by the guest, which is UDP behaving as
designed.

**A forward serves an org-visible sandbox only.** The public port carries no
credential — there is no request to authenticate and nothing to ask a token of
— so the row has to say that a caller with no credential may be let in, which
is what `visibility: "org"` means and `private` does not. `POST /forwards` on a
private sandbox is refused with 409 `sandbox_private`, and an existing forward
stops carrying traffic the moment its sandbox goes private (and serves again
when it is shared back). Reach a private sandbox through its preview URL, which
takes a token, or share it with the org first.

**Docker nodes forward only ports declared at create.** A container publishes
what it was started with, so `POST /forwards` for anything else is refused with
409 `port_not_declared` rather than binding a listener that can never connect.
Firecracker reaches any guest port.

**A connection to a paused sandbox wakes it**, and the node holds the request
open until the sandbox is running and its app has bound — up to ~20 s for a
docker restore. Clients should use a connect timeout that allows for it.

### Restricting preview access by source network

An org can be given source CIDRs, after which preview requests from anywhere
else get the same 404 as a sandbox that does not exist (never a 403, which
would confirm the sandbox is real):

```
PUT /v1/admin/orgs/<org>/preview-sources   {"cidrs": ["100.64.0.0/10"]}
GET /v1/admin/orgs/<org>/preview-sources
```

Operator credential only, and no CIDRs means no restriction. A tailnet customer
passes `100.64.0.0/10` plus their exit-node addresses; no Tailscale dependency
is involved.

**This gate is on the orchestrator path only.** A peer that can reach a node's
own API port reaches `/preview/...` without passing it, so the gate is worth
setting only where the nodes are already unreachable from the internet (see
*Nodes must not be reachable from the internet* below, and `BIND_ADDR` on the
node). That is why there is no dashboard control for it yet.

### Setting an org's base egress policy

Every create in an org starts from the org's base policy and may only narrow
it: the request's allow list is intersected with the base one and the deny
lists are unioned. Without an override the base is the template's own default.

```
PUT /v1/admin/orgs/<org>/egress   {"network": {"allow": ["203.0.113.7", "198.51.100.0/24"]}}
PUT /v1/admin/orgs/<org>/egress   {"network": null}     # clear it
GET /v1/admin/orgs/<org>/egress
```

Operator credential only. Addresses and CIDRs are enforced in the sandbox's
network namespace; domain rules are refused here as they are on create, because
nothing reads SNI or Host yet. The allow list is public-only — a private,
loopback or link-local entry is a 400 — and `0.0.0.0/0` is legal in `deny`
alone.

A policy with a `deny` list and no `allow` list denies **all** egress: there is
nothing left for the deny entries to exclude. Both this route and the create
response say so in `network_advisories`; the sandbox still boots, so read them.

Only a runtime that enforces egress accepts one. A create for an org with a
base policy answers `501 egress_not_enforced` on a node that cannot apply it
(every docker node), rather than booting a machine that reads as confined and
is not.

A **domain** entry needs one thing more than an address entry: the name layer,
which reads the TLS SNI on 443 and the HTTP `Host` on 80 inside the sandbox's
own namespace. `GET /v1/capabilities` reports it as `egress_domains`, placement
routes such a create onto a node that has it, and a fleet where nothing does
answers `501 egress_not_enforced` before placing anything. **A domain entry
matches on 80 and 443 and nowhere else**: no other port carries a name, so
everything else is judged on address and a domain rule written for a database
port filters nothing. `network_advisories` says so on a domain-only allow list.

---

## Environment

### Control plane (`cmd/orchestrator`)

| variable | required | notes |
|---|---|---|
| `ORCHESTRATOR_API_KEY` | **yes** | the operator/fleet key. Also the secret behind browser preview tokens — unset means preview gating **fails closed**, not off |
| `ORCHESTRATOR_TOKEN` | **yes** in a fleet | signs `X-Org-Override` and the node-hop preview token; without it those relays refuse. Nodes no longer present it — each presents its own derived heartbeat token |
| `NODE_KEY_SECRET` | **yes** in a fleet | control-plane only, **never** on a node. Derives every per-node credential. The orchestrator **refuses to start** without it when `ORCHESTRATOR_TOKEN` is set |
| `ORG_ASSERT_KEY` | **yes** in a fleet | Ed25519 seed (`openssl rand -hex 32`) that signs `X-Org-Override`; nodes get only the public half at join. The orchestrator **refuses to start** without it when `ORCHESTRATOR_TOKEN` is set. See *The two secrets that stop one node compromising the fleet* |
| `VAULT_SECRET_KEY` | **yes** in a fleet | encrypts tenants' provider API keys (`vault_secrets`); control plane only, **never** on a node (`openssl rand -hex 32`). The orchestrator **refuses to start** without it when `ORCHESTRATOR_TOKEN` is set, unless `ALLOW_PLAINTEXT_VAULT=1` (dev only). See *At-rest keys* |
| `SANDBOXD_DATABASE_URL` | **yes** in a fleet | Postgres. Without it the orchestrator still boots but serves **no** projects, threads, vault or launch stats, and **admission is disabled** — see the warning below |
| `PORTAL_SANDBOX_DOMAIN` | for previews | the wildcard domain. Unset disables host ingress entirely, and with it custom domains (`GET /v1/capabilities` reports `custom_domains: false`) |
| `TRUSTED_PROXY_CIDRS` | with a TLS terminator | comma-separated CIDRs whose `X-Forwarded-For` is believed. Unset means the header is ignored everywhere |
| `PILLDROPS_REQUIRE_SECURE_HOP` | after the fleet has rolled | `1` refuses to register or forward to any node that has not advertised a TLS pin. Default off — see *Encrypting the orchestrator→node hop* |
| `FORWARD_INGRESS` | on **exactly one** replica | `1` makes this replica bind the public ports of every port forward; `remote` on the API-only replicas declares that another one does. Unset everywhere means no ingress exists, and the forwards API refuses creates rather than serving an address nothing answers on — see *Raw port forwarding* |
| `FORWARD_PUBLIC_HOST` | for port forwards | the hostname the endpoints are published under. Defaults to `PORTAL_SANDBOX_DOMAIN`; with neither set, `POST /forwards` is refused with `no_forward_host` |
| `FORWARD_PORT_RANGE` | no | `lo-hi` public port range, default `40000-60000`. An unparseable value falls back to the default |
| `ORCHESTRATOR_ROLE` | no | `edge` runs a replica that serves **only sandbox traffic** — previews (path and host form), the agent origin, customer domains — from the shared Redis routes. Needs `REDIS_URL` and the same `ORCHESTRATOR_API_KEY`, `ORCHESTRATOR_TOKEN`, `PREVIEW_SECRET`, `SANDBOXD_DATABASE_URL` and `PORTAL_SANDBOX_DOMAIN` as the API replicas; it runs no API, dashboard, reconciler or background job. Point `*.<domain>` at edge replicas and `api.`/the console at the others, and preview traffic scales — and keeps serving — independently of the API (verified: previews answered 200 through an edge with every API replica stopped) |
| `ORG_RATE_PER_SEC` | no | requests per second per org across every replica (default `40`, `0` off); counted in Redis when `REDIS_URL` is set. Over it: `429 rate_limited`, `Retry-After: 1` |
| `ORCHESTRATOR_ARTIFACT_STORE` | no | `s3` keeps template builds, named snapshots and move captures in a bucket (with `ORCHESTRATOR_ARTIFACT_S3_*`, below); every replica shares it and nodes read it directly. Unset uses `ORCHESTRATOR_ARTIFACT_DIR`. The process **refuses to start** if it cannot list the bucket. See *Object storage* |
| `ORCHESTRATOR_ARTIFACT_S3_ENDPOINT` · `_BUCKET` · `_REGION` · `_ACCESS_KEY` · `_SECRET_KEY` · `_SESSION_TOKEN` · `_VIRTUAL_HOST` | with `…_STORE=s3` | the bucket. Same meaning as the node's `SANDBOXD_ARTIFACT_S3_*` |
| `ORCHESTRATOR_ARTIFACT_DIR` | without a bucket | a local directory for the same objects. One replica's disk: replicas on other hosts do not see it, and every node read goes through this process. Unset **and** no bucket means publishing is refused |
| `ORCHESTRATOR_ARTIFACT_DIRECT_SECONDS` | no | default `900`. A node's artifact read is answered with a redirect to a presigned bucket URL valid this long, so the bytes go bucket → node. `0` proxies them through this process (gzipped). Only takes effect with a bucket, and nodes must be able to reach its endpoint |
| `PREVIEW_CACHE_MS` | no | default `0`. How long a replica caches the preview gate's store reads. `0` means making a sandbox private, or destroying it, retracts its preview links on the very next request; a positive value trades that retraction latency for fewer database reads under preview load |
| `PORT` | no | default `9090` |

### Node (`cmd/sandboxd-fleet`)

| variable | required | notes |
|---|---|---|
| `ORCHESTRATOR_URL` | **yes** | makes the node clustered. Also switches on the fleet-ingress gate (below) |
| `ORCHESTRATOR_TOKEN` | **yes** | **this node's own heartbeat token** (`node_token` from join), NOT the control plane's fleet-wide secret. The name is historical; a node has not held the fleet secret since `NODE_KEY_SECRET` became mandatory |
| `NODE_ID`, `NODE_URL` | **yes** | identity and the address the orchestrator dials |
| `NODE_API_KEY` | **yes** | registered as an operator key so orchestrator calls authenticate. The orchestrator DERIVES it as `HMAC(NODE_KEY_SECRET, "id\|zone")`, so it must match exactly — do not invent one (see *Joining a node* below) |
| `SANDBOXD_DATABASE_URL` | **yes** | the same Postgres as the control plane |
| `SANDBOXD_SANDBOX_DOMAIN` | for previews | **the same value as `PORTAL_SANDBOX_DOMAIN`, under a different name** |
| `SANDBOXD_PRODUCTION` | recommended | `1` turns on the boot guards below |
| `SANDBOXD_SECRET_KEY` | recommended | encrypts stored env/secrets at rest |
| `PILLDROPS_IMAGE_ALLOWLIST` | **off by default** | comma-separated registry prefixes a caller-supplied `image` may come from. Empty = **any registry**. See *Tenant image trust boundary* |
| `PILLDROPS_IMAGE_REQUIRE_DIGEST` | **off by default** | `1`/`true` requires a caller-supplied `image` to be pinned as `name@sha256:…` |
| `PILLDROPS_REQUIRE_SECURE_HOP` | after the fleet has rolled | `1` makes this node refuse to heartbeat to a plaintext `ORCHESTRATOR_URL`, and refuse an org assertion with no replay nonce. Default off |
| `SANDBOXD_ARTIFACT_STORE` | for backups and pause archives | `s3` (with `SANDBOXD_ARTIFACT_S3_*`) gives the node a writable bucket; the clustered default, `controlplane`, is read-only. See *Backups and node loss* and *Object storage* |
| `SNAPSHOT_ARCHIVE` | recommended with a bucket | `bucket` archives every pause to the node's bucket, so a paused sandbox **resumes on another node when its own is gone**. Needs `SANDBOXD_ARTIFACT_STORE=s3`; the node refuses to start otherwise. See *When a node dies* |
| `SNAPSHOT_ARCHIVE_DIR` | no | the older alternative: pause snapshots copied to a directory, expected to be a network volume. Restorable only by a node with the same id |
| `SANDBOXD_LAZY_MEMORY` | no | default on (`0` off). A sandbox restored from its archive on another node boots **before its memory is downloaded**: pages are fetched from the bucket as the guest touches them and the rest prefetched. Needs Firecracker `v1.17` or later on a 6.x+ host kernel (see the pin table). *When a node dies* describes the one window it adds |

> **Nothing configures the node's TLS certificate.** It is generated on first
> boot into `SANDBOXD_DATA` as `node-tls.crt` / `node-tls.key` and advertised in
> the heartbeat. There is no CA, no DNS name and no renewal. See *Encrypting the
> orchestrator→node hop*.

> **The two domain variables are the same setting with two names.** The control
> plane reads `PORTAL_SANDBOX_DOMAIN`; a node reads `SANDBOXD_SANDBOX_DOMAIN`.
> Setting one and not the other gives you a tier that routes preview hosts and a
> tier that does not, which presents as previews working through one path and
> 404ing through another. This is a wart, recorded here because it will bite
> before it gets fixed.

---

## Nodes must not be reachable from the internet

A node's API is the control plane's to call, not a customer's.

Since the fleet-ingress gate, a **clustered** node refuses any non-operator key
on `/v1` (`internal/httpapi/auth.go`), so a tenant who finds a node's address
gets a 403 rather than a working API. That closes the bypass where a tenant
collected their concurrency allowance once *per node*, because every capacity
check a node makes is node-local.

Treat that as defence in depth, not permission to expose them. Put nodes on a
private network. `SANDBOXD_ALLOW_DIRECT_TENANT_API=1` re-opens the gate for an
operator who exposes nodes deliberately and accepts that per-node caps become the
only caps.

---

## Joining a node

Do not hand-write a node's environment. Both of a node's credentials are
derived from `NODE_KEY_SECRET`, which lives on the control plane and is never
shipped to a node:

| node env | derivation | used for |
|---|---|---|
| `NODE_API_KEY` | `sf_node_` + `HMAC(NODE_KEY_SECRET, "<id>\|<zone>")[:32]` | orchestrator→node calls |
| `ORCHESTRATOR_TOKEN` | `sf_hb_` + `HMAC(NODE_KEY_SECRET, "hb\|<id>\|<zone>")[:32]` | this node's heartbeat bearer |

Get either wrong and the node 401s — on `/internal/heartbeat` for the second,
on every orchestrator→node call for the first. The DSN and secrets have to match
the control plane's too.

Two supported paths:

```bash
# One-time token (preferred). The token IS the authorization, so the redeem
# endpoint is deliberately not behind the API key — and it hands out the Postgres
# DSN and the shared secret, so it must only ever be redeemed over HTTPS.
curl -X POST https://<domain>/v1/join-tokens -H "Authorization: Bearer $ORCHESTRATOR_API_KEY"
# → {"token": "...", "bootstrap_cmd": "...", "expires_at": ...}
#   Paste bootstrap_cmd on the new host; it prompts for the token on /dev/tty
#   with echo off, so the credential never enters shell history or /proc/<pid>/cmdline.

# For config management, redeem the token yourself and read the fields you need.
# This is the same call install.sh makes, and it is single-use:
curl -X POST https://<domain>/v1/join \
  -H 'Content-Type: application/json' \
  -d '{"token":"<token>","suggested_id":"node-5","zone":"eu"}'
# → {"node_id", "node_api_key", "node_token", "orchestrator_url",
#    "postgres_dsn", "secret_key", "zone", "org_assert_public_key"}
```

---

## Host setup, per node

### Provisioning a bare host

`scripts/provision-node.sh` takes a bare KVM box to "can boot a microVM". Run
it as root from a checkout, on a host that has `/dev/kvm`; it is idempotent, so
a re-run resumes instead of duplicating work.

```bash
cd ~/pdbuild && sudo BUILD_DIR=$PWD TEMPLATES=base bash scripts/provision-node.sh
```

It installs Docker + squashfs-tools, a Go toolchain in `/usr/local/go`,
Firecracker and its jailer in `/usr/bin`, the guest kernel at
`/var/lib/sandbox/vmlinux`, host networking via `scripts/setup-host.sh`, and a
template rootfs under `/var/lib/sandbox/templates` (`TEMPLATES=` lists them,
comma-separated, default `base`; set but empty builds none), `envd` and the
guest CLI in `/opt/pilldrops/bin` (the Docker runtime bind-mounts both into
every sandbox; without them a docker sandbox has no exec API or terminal), and
the `sandboxd` and `sandbox-egress-restore` units from `packaging/`, enabled
but not started. A re-run leaves an installed unit alone, so set the node's
environment and other overrides in a drop-in (`systemctl edit sandboxd`
writes `/etc/systemd/system/sandboxd.service.d/override.conf`) rather than by
editing the unit. A re-run warns when an installed unit differs from the
packaged one; to pick up the packaged version, keep your changes in a drop-in,
delete the installed unit and re-run. Go is there on purpose:
`internal/vm` is `//go:build linux` and runs zero tests on macOS, so live work
means `go build` / `go test -c` **on the node** rather than cross-compiling and
copying binaries across.

The pins, and their sha256s, live in the script itself — not in a comment here,
because the script this one replaces (`deploy/firecracker-setup.sh`) was deleted
and its pins then had to be reconstructed from git history:

| pin | value | why it is pinned |
| --- | --- | --- |
| Firecracker + jailer | `v1.17.0` | verified against the release's own `.sha256.txt`. **Not `v1.7`:** its userfaultfd library rejects the ioctl flags a 6.x+ host kernel reports (`Unrecognized ioctl flags`), so lazy memory cannot run on it. Snapshots do not carry across the bump — see *Upgrading Firecracker* |
| guest kernel | **`5.10.223`** (`firecracker-ci/v1.10/<arch>/vmlinux-5.10.223`) | **load-bearing.** A 6.1 kernel cold-boots the VM and then envd never comes up — the guest contract does not match. The CI bucket publishes no checksum, so the script's own self-computed sha256 is the trust anchor and every fetch fails closed. |
| Go | `1.25.13` | must satisfy `go.mod`; checksum from go.dev/dl |

The guest CLI (`bin/pilldrops-linux`) comes from the sibling `pilldrops-cli`
repo, as in the Makefile's `cli-linux`; point `CLI_REPO=` at it or drop a
prebuilt binary in `bin/`.

Verify by using the host, not by listing packages: start a side-car sandboxd
(own `PORT`, own `SANDBOXD_DATA`, shared `SNAPSHOT_DIR`/`OVERLAY_DIR`/
`KERNEL_PATH`), `POST /v1/sandboxes`, exec `uname -r` — it must say `5.10.223`
from inside the guest — then `DELETE` it and check no `firecracker` process
survives.

**Firecracker nodes need no firewall setup script by default.** Since
add-sandbox-netns, every sandbox boots in its own network namespace
(`FC_NETNS`, on unless set to `0`) and the node writes its own rules: the
egress allow-list, metadata/RFC1918/CGNAT blocks and inter-sandbox drop go
inside each namespace at slot creation, the host-side `sandboxd_netns` nft
table (NAT plus sandbox→host drop) and `ip_forward` are asserted at boot, and
`verify-egress.sh --netns` / the boot preflight check exactly those. The host
needs the `nft` and `nsenter` binaries present (packages `nftables`,
`util-linux`) and the jailer beside the firecracker binary — a host
provisioned for Firecracker (/dev/kvm + binary + kernel) whose init fails on a
missing dependency **refuses to start**, naming the binary. It deliberately
does NOT fall back to Docker: that fallback ran tenants on a shared kernel
because a firewall tool was missing (measured 2026-09-04). A host that wants
Docker says so with `SANDBOXD_RUNTIME=docker`.

The scripts below remain required for **Docker nodes**, and for Firecracker
nodes held on the shared-bridge fallback with `FC_NETNS=0`:

```bash
sudo ./scripts/setup-host.sh           # FC bridge fallback: bridge, NAT, egress + INPUT rules
sudo ./scripts/setup-docker-egress.sh  # Docker: DOCKER-USER egress + sandbox->host INPUT
./scripts/verify-egress.sh --docker    # or --firecracker (bridge fallback) / --netns
```

These install the rules that stop a sandbox reaching cloud metadata
(`169.254.0.0/16`), other RFC1918 ranges, the node itself, and other sandboxes.
**They are host setup, not something the node applies to itself** — a node whose
rules were never applied, or did not survive a reboot, looks exactly like one
that is protected. (That failure mode is specific to these host-owned rules;
it is the reason the netns default moved them into the node.)

`setup-docker-egress.sh` keys its rules on `sandboxd-net`'s subnet, creating the
network with the runtime's options if `sandboxd` has not started yet. Re-run it
after upgrading a docker node: node preflight fails until the current rules are
installed (see [SECURITY.md](../SECURITY.md#upgrading-an-existing-fleet)).

Both scripts `iptables-save` to `/etc/iptables/rules.v4`, and
`sandbox-egress-restore.service` replays that file before `sandboxd` starts.
`sandboxd.service` declares `Requires=sandbox-egress-restore.service`, so a node
whose restore unit is missing or failing **does not start sandboxd** rather than
starting it with unrestricted tenant egress. `scripts/provision-node.sh` installs both units from `packaging/` and enables
them without a `|| true`, then asserts `systemctl is-enabled` says `enabled`.

The boot preflight **enforces by default**: a missing *or mis-ordered* rule makes
the node refuse creates and drop out of the fleet, instead of quietly running
without a boundary. It logs every probe, passes included, so a clean boot is
visibly distinct from one where preflight never ran. `SANDBOXD_PREFLIGHT_ENFORCE=0`
opts out and logs a warning on every boot — set it only on a box you are the sole
tenant of.

---

## Boot guards

A production node refuses to start rather than run unsafely:

| guard | trigger |
|---|---|
| `ProductionDBError` | `SANDBOXD_PRODUCTION=1` with a non-Postgres `SANDBOXD_DATABASE_URL` |
| `IsolationGuardError` | no isolated runtime available (neither Firecracker nor Docker), **or** a host provisioned for Firecracker whose init failed (missing `nft`/`nsenter`/jailer — the error names it; degrading such a host to Docker is refused). `SANDBOXD_DEV=1` overrides — local dev only |

> `SnapshotGuardError` used to be a fourth row here, refusing
> `FC_SNAPSHOTS`/`FC_MEM_SNAPSHOTS` on a production node. It was **removed
> 2026-09-03** (Phase 4b §5.5) once every reason it had ever stated had a
> measurement rather than an argument behind it; see docs/runtimes.md
> §firecracker. Golden-snapshot restore (`FC_SNAPSHOTS`) is **on by default**
> since 2026-09-27; `FC_SNAPSHOTS=0` opts a node out.
>
> **Templates baked with an old envd cold boot until rebuilt.** A golden VM boots
> with no credential by design, and an envd baked before Phase 4b §1 has no vsock
> credential channel and so refuses to bind. The node reads the baked envd's
> protocol level out of the rootfs (`unsquashfs -cat`, squashfs-tools) before
> booting a golden VM, logs `golden_snapshot_refused` for such a template, and
> serves it cold boots — no startup stall. Goldens build in the background, one
> template at a time, so a node serves (cold) creates from its first second. `FC_MEM_SNAPSHOTS` is a *different* feature (per-sandbox pause/resume)
> and does not build golden snapshots at all.
>
> **`FC_MEM_SNAPSHOTS` is ON by default** as of this change. Per-sandbox
> pause/resume, fork and checkpoints all ride it, and the resume path it gates
> was proven live before the flip: 15 pause/resume cycles with a 90 s gap and 0
> failures, 24 clean cycles overall. What the earlier intermittent failures
> turned out to be was the *shared node*, not the code — two Firecracker daemons
> sharing namespace slots delete each other's veth links, and a jailer path deep
> enough to push the vsock socket past 107 bytes fails credential pushes while
> plain boots still succeed. Give every co-hosted daemon its own
> `FC_NETNS_SLOT_BASE` and keep `FC_JAILER_DIR` short (`/srv/<name>`, not a deep
> path under `/var/lib/...`). `FC_MEM_SNAPSHOTS=0` opts a node back out, and
> then pause is a 500 and the idle timers never fire.
>
> **Not every intermittent resume failure was the shared node.** One of them is
> the guest: a VM restored from a memory image intermittently stops executing
> anything a timer drives — a 1 Hz timer inside it managed one tick in 303
> seconds while its clock ran normally — and the stall lasts as long as that VM
> had been alive when it was snapshotted, measured 1:1 up to 303.9 s on a
> 300-second-old sandbox, on an idle node, with no standby anywhere in its life.
> It is intermittent, and gets likelier with age: 0 of 3 at 30 s, 3 of 8 at
> 86 s, 4 of 5 at 300 s. The node waits it out rather than declaring the guest
> dead — a push that Firecracker acked and nothing answered proves the guest is
> alive — so such a resume is **slow**, up to a quarter of an hour, not failed.
> Before that rule a five-minute-old sandbox simply could not be resumed: the
> same snapshot stalls the same length every time, so all three attempts failed
> identically at the 120 s deadline. `hibernate_timeout` (4 h) drives this path
> on every Firecracker node, so it is not a standby-only risk: a long-lived
> sandbox is the *worst* case, not a safe one, and a resume of one can take
> minutes.
>
> **A rebaked template's goldens are rebuilt on their own.** A golden is a
> memory image captured against one exact `rootfs.squashfs`, and the restored
> kernel keeps that file's block layout in its page cache: restored over a new
> squashfs under the same id, it fails with `SQUASHFS error: Failed to read
> block ...: -5`, surfacing as `envd not ready: ... open /root: input/output
> error`. Each golden records the digest of the image it was taken against, so
> after a rebake the node stops serving it, cold boots the next creates and
> builds new goldens in the background. A golden from before the digest was
> recorded is rebuilt once the same way.
>
> **Golden restore serves exactly one storage size.** A golden snapshot bakes
> its overlay at 20480 MB (`vm.DefaultOverlayMB`; the node advertises it as
> `warm_restore_storage_mb` in `GET /v1/capabilities`), and it cannot be
> resized on restore — the resumed kernel holds the mounted ext4 state in its
> restored memory. A create that resolves to **any other** writable-disk size
> (a per-plan storage cap, a template's `StorageMB`, an explicit request) is
> served correctly but **cold boots** (~1 s instead of ~40 ms). On a
> deployment whose plans cap storage below 20480 MB this means the warm path
> effectively never fires; that is the intended trade (before 2026-09-04 the
> restore path silently ignored the declared cap — a 5x bypass). Per-size
> goldens were considered and rejected: one full memory image + overlay per
> template per size, built serially at startup, against a plan-driven size set
> the node cannot enumerate.
>
> The default moved 5120 -> 20480 MB with the `docker` template, whose images do
> not fit in 5 GB. Note what that changes and what it does not: overlays are
> sparse, so a 20 GiB golden costs allocated blocks, not 20 GiB (measured: 32 MiB
> allocated on a fresh sandbox). But **every template whose catalogue row
> declares its own `StorageMB` still resolves to that number** and therefore
> still cold boots — `internal/sandbox/policy.go` fills an unset `storage_mb`
> from the template before the VM layer is reached, so `DefaultOverlayMB` is
> reached only by the golden bake. Templates meant to warm-restore must declare
> 20480 or nothing.

---

## Tenant image trust boundary

`POST /v1/sandboxes` accepts an `image` field. That is a product feature —
"bring your own image" — and it means a caller chooses code that runs on your
node. Two node-local controls bound it, **both off by default**, and this
section exists because until now they were discoverable only by reading
`internal/httpapi/imagepolicy.go`.

```bash
# Only these registries, and only digest-pinned refs.
PILLDROPS_IMAGE_ALLOWLIST=ghcr.io/acme,docker.io/library
PILLDROPS_IMAGE_REQUIRE_DIGEST=1
```

**Where each control applies.**

| | applies to | does not apply to |
|---|---|---|
| allowlist / digest pin | `image` on the create body | catalogue template images, builtin service images, custom images built through `POST /v1/images/build` |

The exclusion is deliberate and not a gap: an allowlist is scoped to
attacker-influenced input. Applying it to platform pulls would stop a cold node
fetching `python-3.11` the moment an operator switched the allowlist on.

**What the allowlist does and does not buy you.** It bounds *where* an image
comes from, not *what is in it*. `ghcr.io/acme` on the list does not stop a
typosquat within `ghcr.io/acme`, and it says nothing about the image's contents.
Digest pinning is the control that bounds *what*: with it on, an allowed tag
cannot be re-pointed under you.

Matching is on a component boundary, so `ghcr.io` does **not** match
`ghcr.io.evil.com`. `ghcr.io` and `ghcr.io/` behave identically.

**Both settings are logged at startup, including when they are off.** Two lines,
one per enforcement layer:

```
level=WARN msg="tenant image policy: NO registry allowlist — a create may name an image from any registry" set=PILLDROPS_IMAGE_ALLOWLIST require_digest=false
level=INFO msg="tenant image backstop resolved" component=docker allowlist_entries=0 require_digest=false
```

If you set `PILLDROPS_IMAGE_ALLOWLIST` and still see `allowlist_entries=0` or
the WARN line, the variable did not reach the process — that is the failure a
typo produces, and it is the reason these log on the OFF path at all.

**What a supplied image runs as.** Not one answer, and the difference is
visible to your customers:

| create | PID 1 | `docker exec` / files / terminal |
|---|---|---|
| `image` only (`--image python:3.11-slim`) | root, running our own `sh -c` prologue | **uid 1001** |
| `image` **+** `ports` (`--image postgres:16 --ports 5432`) | **root** — the image's own entrypoint | root |

The second row is the one to understand before enabling BYO-image. When a
request supplies ports, we run *the image's own entrypoint*, and that entrypoint
has to start as root: `postgres` runs `initdb` and chowns `PGDATA`, `nginx`
binds `:80`, and each drops privilege itself afterwards. We cannot pick a uid
for a process whose startup we do not control, and forcing one breaks first boot
outright. An allowlisted image does not change this — provenance says nothing
about what an entrypoint needs.

That class is still under `--cap-drop=ALL` plus eight capabilities,
`no-new-privileges`, `--pids-limit`, a per-tenant network and the disk cap. It
sits exactly where *every* template sat before the privilege drop landed, not
below it. If that is not acceptable for your deployment, the control is the
allowlist: restrict which images can reach it.

---

## Two ways a deploy looks fine and is not

**A control-plane replica with no store serves traffic with admission off.**
`store.Open` failing is fatal on a node but only *logged* on the orchestrator,
which then boots store-less — and `admitBody` returns "allow" when the store is
nil. Watch the boot logs for a store error on any replica; a fleet where one
replica lost the race enforces no fleet-wide concurrency cap at all.

**Restarting a node destroys every Firecracker sandbox on it.** The VMs survive
the daemon — they are separate processes — but nothing can re-address them
afterwards: `Boot` never passes the daemon's sandbox id down to the vm layer,
the pairing between the two ids lives only in memory, and
`fcRuntime.RecoverOrphans` returns nil. So `RecoverOrphans` reaps them on the
way back up, logging `killing_orphan` with the reason.

This is deliberate as of the orphan-recovery fix — the alternative is a live
microVM holding its RAM that no API call can reach and no TTL will ever expire
— but plan around it: **a `sandboxd` restart or crash is a full loss of that
node's running sandboxes.** Drain a node before restarting it. Docker-backed
nodes are not affected in the same way; `RecoverOrphans` there returns instances
the daemon re-registers.

**Admission binds in exactly one of three states.** This is measured, not
described — `TestAdmissionInTheThreeStatesAnOperatorCanBeIn` produces it, and
`go test ./internal/orchestrator/ -run ThreeStates -v` reprints it:

```
  state                        | at the cap? | admitted | what the caller sees
  -----------------------------+-------------+----------+---------------------
  no preset assigned           |  50 running |   true   | (no refusal — create proceeds)
  preset 'free' (max 5)        |   5 running |   false  | plan 'free' allows 5 concurrent
                               |             |          | sandboxes and you are already at
                               |             |          | the limit — pause or destroy one
  same org, database DOWN      |   5 running |   true   | (no refusal — create proceeds)
```

Only the middle row enforces anything. The other two are deliberate, and whether
they are *correct* depends on something the code cannot see:

| | self-hosted | multi-tenant |
|---|---|---|
| **no preset assigned** | right. You never ran `setplan` and were not asking to be capped on hardware you own. | every org is **uncapped from signup** until a human runs `setplan`. That is the default state of a new account. |
| **database unreachable** | right. A blip should not stop your own box launching sandboxes. | anyone who notices a failover gets a window with **no concurrency limit** — during the outage your fleet has least headroom. |

Same code, same lines. The difference is entirely whether an untrusted party
holds an API key and can act on it.

**If you are running multi-tenant, do both of these:**

```bash
# 1. Assign a preset to EVERY org, including new ones. Until you do, they are
#    unlimited — see the first row above.
./bin/setplan -db "$SANDBOXD_DATABASE_URL" -org acme -plan pro

# 2. Alert on the store-error path. There is no config switch for it yet
#    so monitoring is the control you have:
#    grep the orchestrator's logs for "concurrency slot reservation failed"
#    and for a store-open failure at boot.
```

**`SANDBOXD_MODE=multi-tenant` now makes this a deployment decision.** It
changes the first and third rows: with it set, an org with no assigned plan is
**refused** rather than uncapped, and a store error or unopened store is a
**503 + `Retry-After`** rather than a create that proceeds. The default stays
`self-hosted`, which preserves the table above exactly — turning a database blip
into a total outage for someone running this on their own hardware would be a
worse failure than the one being prevented.

```bash
SANDBOXD_MODE=multi-tenant   # limits are part of the security boundary: fail closed
SANDBOXD_MODE=self-hosted    # default: a blip must not stop your own box
```

`TestMultiTenantAdmissionFailsClosedWhereSelfHostedFailsOpen` prints both
columns side by side; run it with `-v` to see them.

Setting it does **not** address the larger issue in the same area: a node holds
`SANDBOXD_SECRET_KEY` and a full Postgres DSN. (It no longer holds the fleet-wide
`ORCHESTRATOR_TOKEN` — see `NODE_KEY_SECRET` below — but the rest still stands.)
Treat a node's host as holding credentials, not as expendable.

## The two secrets that stop one node compromising the fleet

`ORCHESTRATOR_TOKEN` used to be handed to every node at join. Anything derived
from it, or signed with it, is therefore something **any node can also do**. Two
settings move the important cases onto keys the control plane keeps:

| variable | control plane | node | without it |
|---|:--:|:--:|---|
| `ORG_ASSERT_KEY` | yes | **never** (gets the public half) | any node can forge `X-Org-Override` for **any tenant** |
| `NODE_KEY_SECRET` | yes | **never** | **the orchestrator refuses to start.** It used to fall back, and in that mode any node could derive **any other node's** API key and heartbeat token |

```bash
ORG_ASSERT_KEY=$(openssl rand -hex 32)   # Ed25519 seed; nodes receive the PUBLIC half at join
NODE_KEY_SECRET=$(openssl rand -hex 32)
```

**`ORG_ASSERT_KEY` is required in a fleet, not optional.** This used to say it
fell back to `ORCHESTRATOR_TOKEN` and merely left a hole open. That is no longer
true, and the change that made it untrue is `NODE_KEY_SECRET` itself: the
fallback signs with the control plane's `ORCHESTRATOR_TOKEN`, while a node's
`ORCHESTRATOR_TOKEN` is now its **own per-node heartbeat token**. The two sides
key off different values, so the fallback does not verify — it fails.

Measured on two real nodes: `POST /v1/sandboxes` carrying `X-Org-Override`
returns **403 `invalid, expired, or unsigned org override`** with
`ORG_ASSERT_KEY` unset, and **201** with it set and the public half delivered at
join. If your fleet is on `NODE_KEY_SECRET` and every tenant request 403s, this
is why.

**`NODE_KEY_SECRET` no longer falls back. The orchestrator refuses to start
without it** (`NodeKeySecretGuardError`), and `/internal/heartbeat` returns 503
without it, naming the setting. That is a breaking change for any fleet
provisioned before it; the migration is below.

**Rolling them is not zero-downtime.** `ORG_ASSERT_KEY` switches org assertions
from HMAC to Ed25519 and there is **no dual-accept** — a node that kept
accepting HMAC alongside it would leave the forgery path open behind the fix, so
roll the control plane and its nodes together.

### Migrating an existing fleet onto `NODE_KEY_SECRET`

**If you ran a version before this one, read this — your nodes will 401.**

Until now `NODE_KEY_SECRET` was optional and both per-node credentials fell back
to `ORCHESTRATOR_TOKEN`. Every node holds `ORCHESTRATOR_TOKEN`, so on that
configuration any one compromised node could recompute **every** other node's
`NODE_API_KEY` and heartbeat token, heartbeat as them, re-point their URL and be
handed their sandboxes. There is no way to close that without invalidating the
credentials it produced, so this migration is deliberately not seamless: every
node's credentials change at once, and a node that has not been re-issued is
refused rather than quietly accepted.

Order matters. Doing it the other way round leaves the fleet unschedulable for
longer than it needs to be, and doing step 4 before step 3 means placing work on
nodes that are about to drop out.

1. **Generate the secret and keep it on the control plane.**
   ```bash
   openssl rand -hex 32          # → NODE_KEY_SECRET
   ```
   It must **differ from `ORCHESTRATOR_TOKEN`** — reusing it recreates the exact
   hole this closes. It must never appear in a node's environment, a node unit
   file, or a config-management bundle that a node can read. Whatever rolls your
   control plane out should refuse to start if the two are equal.

2. **Compute each node's new pair before you restart anything**, so the window in
   step 3 is as short as possible. For node `<id>` in zone `<zone>`:
   ```bash
   hm() { printf '%s' "$2" | openssl dgst -sha256 -hmac "$1" -hex | awk '{print $NF}'; }
   NODE_API_KEY="sf_node_$(hm "$NODE_KEY_SECRET" "<id>|<zone>" | cut -c1-32)"
   NODE_HB_TOKEN="sf_hb_$(hm  "$NODE_KEY_SECRET" "hb|<id>|<zone>" | cut -c1-32)"
   ```
   Or skip the arithmetic and mint a join token per node (below); `POST /v1/join`
   returns `node_api_key` and `node_token` already derived.

3. **Set `NODE_KEY_SECRET` on the control plane and restart it.** From here every
   heartbeat is refused with 401 until that node is re-issued, and nodes age out
   of the registry after `NodeTTL` (20s). The fleet is unschedulable during this
   step — keep it short.

4. **Re-issue every node, then restart it.** On each node set
   `NODE_API_KEY` to the new value and `ORCHESTRATOR_TOKEN` to `NODE_HB_TOKEN`
   (**the node's own heartbeat token, not the fleet secret** — this is the step
   people get wrong, because the variable name did not change). Then
   `systemctl restart sandboxd`. Each node reappears within one heartbeat.

   Re-joining with a fresh join token does all of this.

5. **Verify** — not that the config is set, that the property holds:
   ```bash
   # every node back and healthy
   curl -s -H "Authorization: Bearer $ORCHESTRATOR_API_KEY" "$ORCH/v1/nodes" | jq '.nodes[]|{id,healthy}'

   # a node's OLD credential no longer works (this is the fix; it must 401)
   OLD="sf_hb_$(printf '%s' "hb|<id>|<zone>" | openssl dgst -sha256 -hmac "$ORCHESTRATOR_TOKEN" -hex | awk '{print $NF}' | cut -c1-32)"
   curl -s -o /dev/null -w '%{http_code}\n' -X POST "$ORCH/internal/heartbeat" \
     -H "Authorization: Bearer $OLD" -H 'Content-Type: application/json' \
     -d '{"id":"<id>","url":"http://attacker:18080","zone":"<zone>"}'   # → 401
   ```

**Node id/URL takeover is refused too.** A heartbeat that re-points a node's URL
while that node is still heartbeating gets **409** and the registry is left
untouched — it is no longer last-writer-wins with a counter. A node whose IP
genuinely changed is accepted automatically once its previous registration ages
out (`NodeTTL`, 20s); nothing to run, and no flag to override it with.

## Encrypting the orchestrator→node hop

Every orchestrator→node request carries `Authorization: Bearer <NODE_API_KEY>`
— the credential that grants **operator** role on that node — plus injected
environment secrets and the signed org assertion. It used to go out as plaintext
HTTP to a public IP, because that is what `install.sh` and `add-node.sh`
advertise (`http://${PUBLIC_IP}:18080`). One packet capture between the control
plane and a node handed you that node.

**There is nothing to configure.** No CA, no DNS name per node, no certificate
renewal, no WireGuard keys to distribute. Concretely, as a self-hoster:

| you do | the software does |
|---|---|
| nothing on the node | generates `node-tls.{crt,key}` in `SANDBOXD_DATA` on first boot and serves TLS **on the existing API port** |
| nothing to your firewall | the port, `NODE_URL` and every preview URL are unchanged — one socket answers both TLS and plaintext |
| **put your control plane behind HTTPS** (already true on Railway, Fly, or behind any reverse proxy) | the node reports its certificate's public-key fingerprint on its authenticated heartbeat, and the control plane pins it |
| flip one variable when the fleet has rolled | refuses any node that has not |

### Why this and not WireGuard

WireGuard was the other candidate and it loses on the deployment this repository
is published for.

- **The control plane usually cannot run it.** The reference deployment is a
  container PaaS (Railway); so are Fly, Render and Cloud Run. No `NET_ADMIN`, no
  kernel module, no persistent interface. WireGuard would force the control
  plane onto a VM the operator administers — a much larger ask than "put it
  behind HTTPS".
- **It moves the problem to key distribution.** Every node needs the hub's
  public key and the hub needs each node's: a per-node out-of-band step, which
  is exactly the operator burden the join protocol exists to remove.
- **It authenticates the tunnel, not the node.** You would still have to bind a
  WireGuard public key to a node id, which is the same pinning problem solved
  twice.
- **Nothing in the code could prove it was on.** The orchestrator would dial
  `http://10.x` either way, so no guard could tell an operator whether the hop
  was protected — and "the deployment happens to use a tunnel" is precisely the
  claim this change exists to replace with a refusal.

The pin is stronger than a public certificate would have been, for what it is
being asked to do. A CA attests *"this host answers to that name"*; the pin
attests *"this is the key belonging to the node holding node-N's heartbeat
credential"* — which is the question the control plane actually has.

### The trust chain, in one line

Node → orchestrator is HTTPS to a named host with a real certificate; that hop
carries the per-node credential from `NODE_KEY_SECRET`; that credential vouches
for the fingerprint; the fingerprint secures orchestrator → node.

**If the first link is plaintext the rest is worthless**, which is why a node
with `PILLDROPS_REQUIRE_SECURE_HOP=1` refuses to heartbeat to an `http://`
control plane rather than leaving it to habit. (`localhost` is exempt: it
crosses no network.)

### Rolling it out — no flag day

Additive in both directions, so there is **no** second co-ordinated restart
after `NODE_KEY_SECRET`'s. Upgrade in any order:

| control plane | node | result |
|---|---|---|
| old | new | node advertises a pin nobody reads; plaintext, as before |
| new | old | node advertises nothing; plaintext, as before, with a warning |
| new | new | pinned TLS automatically, no configuration |

1. **Roll the control plane and the nodes**, in whatever order suits you.
2. **Watch the rollout finish.** `tls_pinned` in the fleet view is the readout:
   ```bash
   curl -s -H "Authorization: Bearer $ORCHESTRATOR_API_KEY" "$ORCH/v1/nodes" \
     | jq '.nodes[] | {id, tls_pinned}'
   ```
3. **Only when every node reports `true`, enforce.** Set
   `PILLDROPS_REQUIRE_SECURE_HOP=1` on the control plane **and** on the nodes,
   and restart. From then on a node with no pin is refused at registration with
   **426**, so the scheduler never places on it — rather than being scheduled
   and failing the create.

Enforcement is deliberately not the default. Defaulting it on would turn a
control plane that upgraded five minutes before its fleet into a total outage,
which is a worse failure than the window it closes.

### What else this closes

- **A replayed heartbeat is refused (409).** Each beat carries the node's clock
  and must be strictly newer than the last one accepted for that id. Without it
  a captured beat could be re-sent to hold a dead node schedulable, or to
  re-assert an address the node had moved away from. A node that sends no
  timestamp (pre-upgrade) is still accepted; one that has sent one does not get
  a replay treated as legacy.
- **A node's TLS pin cannot be changed under it.** Same liveness rule as the URL
  takeover: refused while the incumbent registration is inside `NodeTTL`,
  accepted automatically once it ages out, so a genuine re-key needs no operator
  action. A heartbeat that simply *omits* the pin does not clear the stored one
  — that is the downgrade an on-path attacker would attempt.
- **An org assertion is now bound to the node it was sent to, and is
  single-use.** `X-Org-Override` was signed, time-bound and route-bound but
  named no target, so a node that *received* one could present it to a sibling
  node and act as that tenant there for the rest of its 60 seconds — a replay by
  an adversary that Ed25519 had already stopped from forging. Both halves are
  refused now, with **403**.

  A pre-nonce assertion (from a control plane that has not upgraded) is accepted
  until `PILLDROPS_REQUIRE_SECURE_HOP=1`, so a node can be upgraded first.

**What this does not do, stated plainly:** a nonce on requests that are already
authenticated by the node's bearer token would be theatre. Anyone who can
capture a request to replay it also holds the bearer and can compose a fresh
one. The org assertion is the only credential on this hop that is
control-plane-signed and node-verifiable, which is why it is the only one that
gets replay protection — and once the transport is pinned, network replay is
closed by TLS itself.

## First run: claim the console

The dashboard has its own accounts, on a single `sandboxd` node and on a control
plane alike. On a server nobody has claimed, `/dashboard` offers a **setup
form** — email and password — and the account it creates is the administrator,
stored in the server's own database (SQLite or Postgres).

It used to ask for `ORCHESTRATOR_API_KEY` pasted into a password box. That works
and has three problems: the key is a bearer credential for the whole `/v1`
surface, so the console's threat model was the API's; it cannot be attributed,
because everyone types the same string; and it cannot be revoked for one person
without rotating it for every SDK caller.

- Setup is closed the moment an account exists — a second attempt is a 409.
  The claim is a single transaction, so two people opening the URL together
  cannot both win.
- Passwords are PBKDF2-HMAC-SHA256, 600,000 iterations, per-account salt.
  Minimum 12 characters.
- The session cookie is signed and carries the account it belongs to; it expires
  after 12 hours. It is no longer the API key — and because it is not, the
  control plane recognises it as an operator credential in its own right. That
  is load-bearing: the cookie used to BE the key, so the auth path used its
  value as a bearer token, and a signed session would otherwise sign you in and
  then 401 every request the dashboard makes.
- Creating the account signs you in. There is no second login step.
- A wrong password and an unknown address answer identically, so the form does
  not enumerate accounts.

`ORCHESTRATOR_API_KEY` still authenticates `/v1` — SDKs and `curl` are
unchanged. It just no longer opens the console.

The API key a node writes to `bootstrap.key` is for SDKs, the CLI and `curl`;
it does not open the dashboard. `make dev` has no sign-in at all: it is
anonymous and bound to localhost.

## The root is the dashboard

`/` redirects to the dashboard. There is no storefront, pricing, terms or
sitemap in this repository: a self-hosted install answers only for the
software it runs. What is served besides the API and the dashboard documents
the software: `/docs`, `/guides/*`, `/sdk` and `llms.txt`.
`PILLDROPS_PAGES_DIR` overrides any of those pages from disk, and
`ANALYTICS_DOMAIN` (with `ANALYTICS_SRC`, default Plausible) injects an
analytics tag into them; both are off unless set.

## At-rest keys, and which tier gets which

| variable | control plane | node | protects |
|---|:--:|:--:|---|
| `SANDBOXD_SECRET_KEY` | yes | yes | sandbox env, function env, webhook secrets |
| `VAULT_SECRET_KEY` | **yes** | **never** | `vault_secrets` — tenants' provider API keys |

`VAULT_SECRET_KEY` is the one at-rest key the control plane keeps to itself, and
it is deliberately absent from the node join bundle. A clustered control plane
**refuses to start** without it; unclustered, connecting a provider key is
**refused** rather than stored unencrypted — which is what used to happen, silently.
**Upgrading:** set it on every orchestrator replica before rolling onto a build
with this guard, or the upgraded replica will not start (see SECURITY.md,
*Upgrading an existing fleet*). `ALLOW_PLAINTEXT_VAULT=1` restores the old behaviour
for local development and should never be set on a shared deployment.

Generate one with `openssl rand -hex 32`.

## Give nodes their own database role

A node is handed a Postgres DSN at join and shares the control plane's database.
Do not hand it the owner role:

```bash
psql "$SUPERUSER_DSN" -v node_password="'…'" -f scripts/postgres-node-role.sql
# then hand nodes: postgres://pilldrops_node:…@host/db
```

That revokes the tables node code never touches — `vault_secrets`,
`admission_slots`, `credit_ledger`, `pricing`, `billing_cursor` — and refuses
DDL. It does **not** make a node harmless: it can still read `users` and every
sandbox row.

## Verifying a release

Do not skip this — see [`releases.md`](releases.md). The short version:
`SHA256SUMS` alone proves nothing, because whoever can replace a binary can
replace the checksums beside it. Verify the Sigstore signature **first**, with
the identity flags, then the hashes.

## Backups and node loss

Two separate things are backed up, and losing sight of that is how a fleet ends
up with only one of them.

### The metadata store

`scripts/backup.sh` dumps it: `pg_dump` when `SANDBOXD_DATABASE_URL` is set,
otherwise SQLite's `.backup`, which takes a consistent snapshot while sandboxd
is running (the database is in WAL mode). It prints the artifact path.

`scripts/restore.sh` puts it back, and **sandboxd must be stopped first** — a
running daemon holds the database open and writes over the restore.

Nothing schedules the dump. Install the timer:

```bash
sudo install -m755 scripts/backup.sh /usr/local/bin/sandboxd-backup.sh
sudo cp scripts/backup.service /etc/systemd/system/sandboxd-backup.service
sudo cp scripts/backup.timer   /etc/systemd/system/sandboxd-backup.timer
sudo mkdir -p /var/backups/sandboxd
sudo systemctl daemon-reload
sudo systemctl enable --now sandboxd-backup.timer
systemctl list-timers sandboxd-backup     # confirm it is armed
```

On Railway, where there is no systemd, add a cron service running
`scripts/backup.sh` with `BACKUP_DIR` on a mounted volume:

```
0 4 * * *  BACKUP_DIR=/data/backups /app/scripts/backup.sh
```

The dump holds sandbox rows, keys and org records. It does **not** hold a
single byte of any sandbox's disk.

### Sandbox disks

A sandbox's disk is captured on demand or on a schedule, to object storage:

```bash
curl -X POST …/v1/sandboxes/$ID/backups                                  # capture now
curl        …/v1/sandboxes/$ID/backups                                   # list
curl -X POST …/v1/sandboxes/$ID/backups/$BKP/restore -d '{"confirm":true}'
curl -X DELETE …/v1/sandboxes/$ID/backups/$BKP
```

The capture is synchronous and the upload is not: the response returns as soon
as the sandbox is running again, with the backup `pending`, and the row turns
`ready` once the bytes are in the store. `paused_ms` on the response is how
long the machine actually stood still — on Firecracker the first capture of a
sandbox writes a full memory image (measured: ~3 s for a 1 GiB guest) and every
later one is a dirty-page diff (~200 ms).

`snapshot_kind` says what a given backup holds: `memory` on Firecracker, where
RAM and running processes come back, and `disk` on Docker, where only the
filesystem does and the start command is relaunched on restore. Read the field
rather than assuming — it is per backup, not per fleet.

**Restore overwrites the sandbox's current disk and there is no undo.** It is
refused without `{"confirm": true}`. The sandbox keeps its id, name, preview
URL, environment and start command; on a `disk` capture it loses its memory and
running processes.

#### This needs an object store

Backups go straight from the node to the artifact store, so that store has to
be one that can hold gigabytes:

```bash
SANDBOXD_ARTIFACT_STORE=s3
SANDBOXD_ARTIFACT_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com
SANDBOXD_ARTIFACT_S3_BUCKET=pilldrops-artifacts
SANDBOXD_ARTIFACT_S3_REGION=auto
SANDBOXD_ARTIFACT_S3_ACCESS_KEY=…
SANDBOXD_ARTIFACT_S3_SECRET_KEY=…
# SANDBOXD_ARTIFACT_S3_VIRTUAL_HOST=1   # bucket as a subdomain; path style is the default
```

Any S3-protocol store works: AWS S3, Cloudflare R2, MinIO. `controlplane` does
**not** — nodes mount it read-only (a writable mount would let one compromised
node replace what every other node boots), so a backup PUT is refused, and a
backup request on such a node is answered with 501 `backups_need_object_store`.
`controlplane` is also the **default on a clustered node**, so a fleet that
sets none of the above has no sandbox-disk backups at all; that is the honest
state, not a soft failure.

`fs` works for a single node, and it is the **default standalone**: the backup
then lands on the same disk as the sandbox, which covers a bad deploy inside
the guest and does not cover losing the machine.

Both defaults are called out at boot — look for `component=artifact` in the
node log:

```
WARN backups are OFF on this node: every backup request answers 501 …
WARN backups are written to this node's own disk (/var/lib/sandbox/state/artifacts) …
```

#### Scheduling and retention

```bash
curl -X POST …/v1/sandboxes/$ID/config \
  -d '{"backups": {"period": "24h", "retention": "7d"}}'
# also:  {"backups": {"cron": "0 3 * * *"}}      {"backups": {"period": "off"}}
```

The floor is 15 minutes, measured from two consecutive fires, so `"5m"` and
`"*/5 * * * *"` are refused identically. `retention` needs a period. The
deployment default is `BACKUPS_RETAIN_DAYS` (7; `0` keeps them forever).

`retention` is stored on the sandbox, so it survives a node restart and comes
back on the config `GET` alongside the schedule. There is no org-level default
between the sandbox and `BACKUPS_RETAIN_DAYS`: a sandbox sets its own or takes
the deployment's.

Expired backups are reclaimed hourly, object first and row second. A backup
another backup depends on is never reclaimed and cannot be deleted: a capture
whose digest matched the previous one stores no bytes of its own and restores
through its parent. `DELETE` refuses those with 409 `backup_referenced`, and a
`pending` one with 409 `backup_in_flight`.

### When a node dies

The control plane reaps a node it has not heard from for two minutes
(`internal/cluster/registry.go`), and its sandboxes go two different ways.

**Running sandboxes are lost.** Their memory went with the host. Asking for one
answers `410 sandbox_lost`; `DELETE` clears the record and keeps a history
entry. Nothing brings them back from an older snapshot: a sandbox that ran
after its last pause has moved on from it, and restoring that pause would hand
its owner a machine silently rolled back.

**Paused sandboxes survive, with `SNAPSHOT_ARCHIVE=bucket`.** Every pause then
archives the full capture (memory, machine state, overlay, root image) to the
node's bucket. When the node is reaped its paused rows stay `paused`; reading
one answers from the record without downloading anything, and the **first
action** on it (a resume, an exec) rehomes it: the control plane asks a healthy
node to take it, that node pulls the archive, boots the sandbox under the same
id and claims the record, and the request carries on there.

Measured on a two-node rig (Firecracker `v1.17`, MinIO, 2026-09-29): a 4 GB
sandbox holding 1 GB of random data resumed on the surviving node in **2.8 s**
with lazy memory and a warm root-image cache (6.1 s cold, 25.2 s with
`SANDBOXD_LAZY_MEMORY=0`), the VM running 51 ms after its load began and its
memory byte-identical; ten sandboxes rehomed at once in 3.0 s.

The rules that keep this honest:

- **An archive is used only if it is the sandbox's latest pause.** Each
  archived pause has a random id stored in both its manifest and the sandbox's
  row (`pause_archive`), and any save of a sandbox that is not paused clears
  it. An archive left behind by an earlier pause — a resume whose cleanup could
  not reach the bucket, say — is refused, and the sandbox is reported lost
  rather than restored older. Verified on the rig.
- **A resume never needs the bucket.** Deleting the archive after a resume is
  tidying in the background. With the bucket down, pause, resume and create all
  keep working; a pause then simply is not archived.
- **A target node must be able to run the capture.** The archive records the
  capturing CPU's features and a node lacking any refuses it; the control plane
  tries the next node. **Firecracker also cannot restore a memory image on a
  host whose TSC frequency differs from the capturing host's unless the CPU has
  hardware TSC scaling** (`grep tsc_scaling /proc/cpuinfo`): measured on two
  Xeon E3 v3 hosts at 3292 and 3392 MHz, captures moved one way only. Keep a
  node pool on one machine model. This applies to moves as much as to rehomes.
- **No archive, no rehome.** A paused sandbox whose node never archived it is
  reported lost when something asks for it.
- **The window lazy memory adds.** Until a lazily restored sandbox's memory has
  finished streaming (roughly memory size ÷ bandwidth to the bucket; ~20 s for
  1 GB on the rig), the node daemon is serving its page faults. If the daemon
  dies in that window, the restarted daemon removes that VM rather than adopt a
  machine that would freeze on its next untouched page, and the sandbox is
  lost. `SANDBOXD_LAZY_MEMORY=0` closes the window at the cost of the download.

A node daemon that merely restarts loses nothing: `SIGTERM` pauses every
running sandbox to local disk and the next daemon restores them; `kill -9`
leaves the VMs running and the next daemon adopts them (verified both ways on
the rig).

Backups (above) are the other path back. With object storage they are off-node
and survive the node — verified by wiping a node's entire local capture
directory and artifact cache and restoring anyway — but they restore onto the
sandbox's current node only.

`scripts/backup-live.sh` is the backup round trip as a runnable check, positive
controls included; point `BKP_API` and `BKP_KEY` at a node to run it.

## Object storage

A bucket is where everything that has to outlive a node lives. Any
S3-protocol store works; three were verified against the same code on
2026-09-29: MinIO (the rig), AWS S3 (`ap-south-1`) and Google Cloud Storage.

| what | where in the bucket | written by |
|---|---|---|
| template builds, named snapshots, move captures | `<build>/<object>` and `<build>/<object>.header` | the control plane (`ORCHESTRATOR_ARTIFACT_STORE=s3`) |
| pause archives | `pause/<sandbox>/manifest.json`, `pause/<sandbox>/files/…` | nodes (`SNAPSHOT_ARCHIVE=bucket`) |
| sandbox backups | see *Sandbox disks* | nodes |
| shared root images | `rfs-<sha256>/rootfs` | whichever stores a capture of that image first |

Point the control plane and the nodes at **the same bucket**. Root images are
only shared within one, and a pause archive's root image is looked up there.

**What is stored.** A memory image and a disk overlay are stored packed: only
the 2 MiB blocks that hold data, with a `.header` block map. That header is
format v3, which a node predating packing refuses rather than misreads, so
update every node before relying on it. A capture's root image is stored once
per content digest and referenced; the first capture uploads it and is checked
against the digest before anything points at it. Measured: a pause archive went
from ~1.8 GB to ~82 MB, and an archived pause from 31 s to 1.8 s across the
rig's network.

**How nodes read it.** With `ORCHESTRATOR_ARTIFACT_DIRECT_SECONDS` above zero
(the default), a node reading through the control plane is redirected to a
presigned URL and downloads straight from the bucket. The node's own key is
stripped on every redirect, so the bucket never sees it. A node configured with
`SANDBOXD_ARTIFACT_STORE=s3` reads the bucket itself.

**Uploads.** Anything over 16 MiB is a multipart upload; a failed one is
aborted, so nothing half-written stays in the bucket or on its bill (checked on
AWS: no in-progress uploads left behind). A store runs at most three large
uploads at once: ten sandboxes pausing together took a node daemon to 1.3 GiB
before that cap and 521 MiB after, in the same time, because the link was the
limit.

**Cleanup.** Destroying a sandbox deletes its pause archive. Shared root images
are swept hourly by the leader: one that no capture or archive references, and
older than an hour, is deleted; a header the sweep cannot read stops it before
any delete.

**Credentials.** Give the fleet a key that can reach this one bucket and
nothing else.

- **AWS S3:** an IAM user whose only policy is `s3:*` on
  `arn:aws:s3:::<bucket>` and `arn:aws:s3:::<bucket>/*`. Endpoint
  `https://s3.<region>.amazonaws.com`, region as created.
- **Google Cloud Storage:** through its S3-compatible XML API — endpoint
  `https://storage.googleapis.com`, region `auto`, and an HMAC key
  (*Cloud Storage → Settings → Interoperability*) for a service account holding
  *Storage Object Admin* on the bucket only.
- **MinIO:** community binaries are no longer published; build it from source
  (`go install github.com/minio/minio@<release>`). Do not publish its port with
  Docker on a host whose firewall is an nftables input chain: Docker's port
  publishing bypasses it.

A bucket name that does not exist fails at boot as *"the bucket does not
exist"*, not as a missing object.

## Upgrading Firecracker

Golden snapshots rebuild themselves: a node that cannot load its goldens after
an upgrade falls back to a cold boot (~1.3 s) and rebuilds them, and creates
are back to tens of milliseconds a few seconds later.

**Every other memory snapshot is tied to the version that took it.** Firecracker
`v1.17` cannot load a `v1.7` pause snapshot, pause archive, move capture or
named snapshot (`bitcode error` in the node log). An upgrade is therefore not
in-place for a node's existing sandboxes:

1. Upgrade one node first and run a create, a pause/resume and a move on it.
2. Cordon the node you are upgrading.
3. Move its running and paused sandboxes to nodes still on the old version
   (`POST /v1/sandboxes/{id}/move`), or have their owners recreate them. Do not
   rely on a daemon restart to carry them over: `SIGTERM` pauses them into
   snapshots the new version cannot read.
4. Run `scripts/provision-node.sh` with the new `FC_VERSION` (it reinstalls
   when `/usr/bin/firecracker` reports another version), restart the daemon,
   and uncordon.
5. Repeat. Sandboxes move freely among nodes on the same version; between
   versions, only toward the version that took the capture.

A named snapshot taken before the upgrade has to be saved again on an upgraded
node before it can be used there.

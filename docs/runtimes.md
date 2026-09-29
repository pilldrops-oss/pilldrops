# Runtimes

What each sandbox runtime this repository ships actually provides, and what its
capability flags do and do not mean.

> **This page is checked against the code.** `TestRuntimeCapabilitiesMatchTheDoc`
> (`internal/runtime/capsdoc_test.go`) parses the table below and compares every
> row against the runtime's own `DeclaredCaps()`. It fails in both directions: a
> capability changed without the doc, a runtime in the code with no row here, and
> a row here for a runtime that no longer exists. The prose is not checked — it
> carries what a boolean cannot — so read the table as pinned and the prose as
> reviewed.

## The declaration

<!-- BEGIN CAPS TABLE -->
| runtime | isolation | snapshot | multiport | checkpoint | standby | l4forward | nested docker | desktop |
|---|---|---|---|---|---|---|---|---|
| firecracker | yes | yes | yes | yes | yes | yes | yes | yes |
| docker | yes | yes | no | yes | yes | yes | no | yes |
| process | no | yes | no | no | no | no | no | no |
<!-- END CAPS TABLE -->

`opt-in` means the flag is false by default and true only when the runtime's
opt-in environment variable is set; the test asserts both halves, so a
capability that silently became unconditional would fail. **No row is opt-in
today.** Firecracker's `snapshot` and `checkpoint` were, behind
`FC_MEM_SNAPSHOTS`, until that default was flipped on once the resume path had
been proven live. `FC_MEM_SNAPSHOTS=0` still turns both off, and
`TestSnapshotsDefaultOn` pins both halves of *that*.

## What the columns mean

**isolation** — whether the runtime enforces a tenant boundary *at all*. It is
a two-value answer to a three-value question, and `internal/runtime/types.go`
says so in its own comment: *"A runtime setting this to true is making a claim,
not demonstrating one… Setting the bit is not the same as having the
property."* Read `true` as "not the process runtime", never as "safe for
hostile code". §Boundaries below is the answer the boolean elides.

**snapshot** — whether pausing frees the sandbox's memory. `false` means a
pause is a `SIGSTOP`: the process stops, the RAM stays resident.

**l4forward** — whether a raw TCP/UDP port can be published on the platform's
forwarding endpoint and spliced to a guest port (`pilldrops sandbox expose`).
Docker says yes but honours only ports declared at create, the same constraint
`multiport` describes; the process runtime has one fixed loopback port and says no.

**multiport** — whether the `<port>-<id>.<domain>` preview form can reach *any*
port the guest opens, or only the one app port the sandbox declared.

**checkpoint** — whether a sandbox can hold named save points and be rolled
back to one in place, keeping its id, URL, token and address. What a restore
brings back differs by engine and every response says which: firecracker
answers `"snapshot_kind":"memory"` and comes back with the same PIDs, uptime
and listening sockets; docker answers `"snapshot_kind":"disk"`, restores the
filesystem and relaunches the start command, so RAM and processes are lost.
See [checkpoints.md](checkpoints.md).

**named snapshot** — not a column, for the same reason per-sandbox egress
policy is not one: it depends on node configuration rather than on the engine.
Firecracker reports it only with `FC_MEM_SNAPSHOTS=1` **and** `FC_NETNS=1`,
because a new machine stamped out of a capture reuses the tap name and guest
address baked into that capture's memory image, and only a private namespace has
room for a second machine wearing them. Docker reports it unconditionally, disk
only. `GET /v1/capabilities` answers `named_snapshot` on both tiers; the fleet
value is the **OR** over healthy nodes, and placement filters a create from a
snapshot onto a node that reports it. Where `checkpoint` rolls one sandbox back,
this stamps out new ones — see [snapshots.md](snapshots.md).

**standby** — whether an idle sandbox can be frozen *in RAM* and thawed in
milliseconds with its processes and open sockets intact. Independent of
`snapshot`, which is the disk tier: standby keeps the memory resident and needs
no flag, `snapshot` writes it out and frees it. Firecracker freezes the vCPUs
(`PauseVM`); docker uses the cgroup freezer (`docker pause`).

**desktop** — whether the *engine* can host an X server. Both shipped runtimes
can, so the column gates nothing on its own; the value a client acts on is
`has_desktop` on the sandbox, which is this bit **and** the sandbox's image
actually holding the screen software. A sandbox started with `PD_DESKTOP=1` in
its environment serves a noVNC viewer on platform port 6080; without the flag
no display process runs and it costs no memory. See
[template-registry.md](template-registry.md) §Desktop.

**per-sandbox egress policy** — not a column, because it depends on node
configuration rather than on the engine: only firecracker with `FC_NETNS=1`
reports it (`Caps.EgressPolicy`). A create carrying a `network` rule set is
**refused** by any other runtime, since the shared bridge's host chains see only
the post-SNAT source and a policy there would be accepted and silently ignored.
Address and CIDR rules are enforced in that namespace's nft chain. A rule naming
a **domain** additionally needs the name layer (`Caps.EgressDomains`, same
condition): a listener inside the same namespace that reads the TLS SNI on 443
and the HTTP `Host` on 80, decides, and splices without terminating TLS.
**Domain rules apply on 80 and 443 only** — no other port carries a name, so
traffic on one is matched on address alone and a domain rule for 5432 filters
nothing. A rule set naming a domain is refused where the name layer does not
run, never accepted and left unenforced.

**tag networks** — also not a column, and for the same reason: `Caps.TagNetworks`
depends on node configuration. Sandbox-to-sandbox traffic is blocked by default
and stays blocked; a sandbox may name up to eight network tags, and two
sandboxes in the same org on the same node reach each other only when they share
one. Firecracker reports the capability with `FC_NETNS=1`, where the partition is
a rule in the node's `sandboxd_netns` table; every other runtime reports false
and a create naming tags is refused there with 501. An untagged sandbox's
rendered ruleset is byte-identical to a node without this feature
(`TestUntaggedRulesetIsUnchanged`), which is what keeps the default a measured
guarantee rather than a claim.

## Boundaries, which is what actually matters

The `isolation` column says two of these three are equivalent. They are not, and
the difference is categorical rather than a matter of degree.

| | firecracker | docker | process |
|---|---|---|---|
| kernel | **its own** | **shared with the host** | shared |
| escape requires | a Firecracker/KVM vulnerability | **a Linux kernel privilege escalation** | nothing |
| root in sandbox = root on host | no | **yes** (no userns-remap) | yes |
| network | own stack, per-sandbox network namespace with node-written egress rules (default; `FC_NETNS=0`: shared bridge, per-sandbox tap, port isolation) | bridge with `icc=false` | none |
| writable disk | per-sandbox ext4 overlay sized from the declared cap | `--storage-opt size`, **enforced only by some drivers** | host filesystem |
| create env (exec, terminal, login shell, start command) | all four, written to `/etc/template.env` in the guest and never on a command line | all four, `docker run -e` | exec only |
| `POST /config` env reaches open shells | yes, the same file is rewritten | no — the container env is immutable, so it applies per exec from the next command on | per exec |
| run Docker inside the sandbox | **yes** — the `docker` template; the daemon is up before the create returns | **never** (see below) | never |
| intended use | untrusted tenant code | trusted workloads, and hosts without KVM | local development only |

The bolded row is the one to internalise. On the Docker runtime a sandbox's
PID 1 runs as uid 0, and with no userns-remap that is **host uid 0** — so a
kernel local privilege escalation is a node compromise rather than a sandbox
compromise. Firecracker has no equivalent path: the guest's root is root of its
own kernel.

What bounds that root on Docker is real and worth knowing —
`--cap-drop=ALL` plus eight capabilities added back, `no-new-privileges`,
`--pids-limit`, a bridge with `icc=false`, published ports bound to
`127.0.0.1`, and no Docker socket mount. `internal/runtime/docker/caps_test.go`
pins the capability set against the kernel's own `CapBnd` rather than against
the argv, and `scripts/security-suite.sh` re-checks it from outside the process
on a live node. It is a reasonable boundary against ordinary buggy and
moderately hostile code. **It is not equivalent to Firecracker and should not be
used for code you expect to actively attack the kernel.**

That same bound is why a Docker-runtime sandbox can never run a container
daemon of its own. `dockerd` needs `SYS_ADMIN` to mount a cgroup hierarchy and
an overlay filesystem, and `NET_ADMIN` to build its bridge; the eight
capabilities kept (`internal/runtime/docker/docker.go`) are `CHOWN`,
`DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETUID`, `SETGID`, `KILL` and
`NET_BIND_SERVICE`, and neither is among them. There is no Docker socket
mounted either, so there is not even a daemon to talk to. Granting the missing
capabilities would hand a tenant the host kernel, which is the one thing this
runtime's boundary exists to prevent, and the optional gVisor mode
(`SANDBOXD_DOCKER_RUNTIME=runsc`) removes the cgroup and overlay mounts
outright. **This is a property of a shared kernel, not a configuration gap, and
no setting turns it on.** Firecracker owns its kernel and so can host a nested
daemon — that is the `docker` template, and it is refused on every other
runtime at create rather than allowed to boot into something that cannot work.

## Per-runtime notes

### firecracker

Production. Real microVMs: own kernel, own network stack, jailer chroot plus
seccomp.

A create is a **golden-snapshot restore by default** (`FC_SNAPSHOTS`, on unless
set to `0`) at **38.0 ms P50 hot / 122.0 ms P50 warm**, and a **cold boot** —
~1.12 s P50 on the reference host
— for a non-default size, a template whose golden is still building, or one
whose baked envd is too old to restore
(30 samples each, zero failures, every create a restore). Both numbers are with `ENVD_AUTH=1`.

Restore was unreachable in production until 2026-09-03, behind two separate
blocks that are both now gone (Phase 4b):

- `nodeboot.SnapshotGuardError` refused the flag outright on a production node.
  **Removed** in §5.5 — see below.
- `acquireVM` disqualified any create that had a per-sandbox token hash, and
  `ENVD_AUTH` is on by default, so in practice that was every create.
  **Removed** in §5.1.

Every reason the guard ever stated is addressed, each with a measurement rather
than an argument (Phase 4b decision D4 requires this ordering: the guard's
*original* reason turned out to be simply false, and a guard whose reason was
wrong is not retired because someone points that out).

- Until 2026-09-01, "restore pins every restored sandbox to one shared
  overlay". Measured false — `restoreVersion` already created a fresh overlay
  per sandbox. Now also true **positively**: two sandboxes restored
  concurrently from one golden snapshot hold overlays on distinct inodes with
  disjoint contents, neither able to read the other's files, while both still
  read the shared golden payload from their own disks.
- Until 2026-09-03, "a VM resumed from a snapshot carries no token hash on its
  kernel cmdline and envd refuses to bind". envd now binds on the strength of a
  live vsock credential channel (§1) and the node pushes the hash after resume
  (§2, `internal/vm/credpush.go`). Verified: a golden VM booted with no cmdline
  hash reached envd-ready, was snapshotted and restored, **refused** its access
  token before the push and **accepted** it after, with no `envd.token_hash=` on
  the restored guest's `/proc/cmdline`.
- Until 2026-09-03, "a golden snapshot pins the guest's address, so restore
  dials an address the guest does not hold and two concurrent restores
  collide". Fixed in §3. The snapshot turned out to pin four things, not one —
  address, MAC, host tap **name** and overlay **basename** — none of which
  Firecracker v1.7.0 lets a restore override, so the restore claims the baked
  tap name for the length of the load, renames it, rewrites the guest's
  identity over vsock, and bridges last. Verified with two sandboxes restored
  from one golden snapshot **at the same time**, both driven, neither able to
  reach the other, both still reaching the internet. (That machinery now
  belongs to the `FC_NETNS=0` bridge fallback only: on the netns default —
  add-sandbox-netns — every sandbox restores into its own network namespace
  where the snapshot's baked tap/MAC/IP collide with nothing, so the restore
  path has no claim, no rename and no SETNET at all.)
- Until 2026-09-03, "the restored guest gets a freshly `mkfs`'d overlay rather
  than the image its snapshot was taken with". Fixed in §3, proven in §4.1:
  64 MiB of host-generated random data written into the golden image before the
  snapshot came back byte-identical from a restored guest whose page, dentry and
  inode caches had demonstrably been dropped. The same restore against an image
  copied moments *before* that payload existed fails instead, so the read is
  reaching the disk rather than the restored page cache.
- Until 2026-09-03, "no P50 create latency has been measured for a
  snapshot-backed template". Measured — the numbers at the top of this section.

**Two things to know before turning it on.**

1. **Old templates cold boot until rebuilt.** A golden VM runs the template's
   own BAKED envd, and an envd predating Phase 4b §1 has no vsock credential
   channel, so it cannot bind in a VM with no boot credential. Before booting a
   golden, the node reads the baked envd's level (`internal/envdproto`) with
   `unsquashfs -cat` and refuses the build (`golden_snapshot_refused`) rather
   than stalling 60 s to learn the same. The refusal holds until the template's
   rootfs changes; any other failed build is retried after 10 minutes, never on
   every create.
2. **`FC_SNAPSHOTS` and `FC_MEM_SNAPSHOTS` are different features.**
   `FC_SNAPSHOTS=1` turns on golden-snapshot restore (this section).
   `FC_MEM_SNAPSHOTS=1` turns on per-sandbox pause/resume in the runtime
   adapter and does **not** build a golden snapshot — nothing in `nodeboot`
   reads it, so `snapshots/v1` stays empty. `TestSnapshotFlagsAreTwoSeparateFeatures`
   pins the split.
3. **Fork, pause and resume share one memory path** (`internal/vm/memsnap.go`,
   `docs/fork.md`). Every restore turns on dirty-page tracking and records the
   memory file it came from as the sandbox's base; a later fork or pause writes
   a *diff* snapshot (the dirtied pages only, 132 ms for 6 MiB measured) and
   merges it onto a reflink of the base after the VM is back. A full snapshot
   of a file-backed guest faults in every page: 2.1 s for 512 MiB on the
   reference host. Fork needs `FC_NETNS=1` (the child carries the parent's
   baked tap and address) and `FC_MEM_SNAPSHOTS=1`.
4. **Everything the jailer hard-links must share one filesystem** — kernel,
   template rootfs, overlays, `SANDBOXD_DATA` (the artifact cache links into
   the image dir) and the chroot base, now `FC_JAILER_DIR` (default
   `/srv/jailer`). Put them on XFS with `reflink=1` or btrfs and the overlay
   and memory copies a fork makes are free; ext4 works but copies for real.
   Keep `FC_JAILER_DIR` short: the API socket path
   `<dir>/firecracker/<id>/root/run/<id>.sock` must fit in 107 bytes, and the
   node refuses to start otherwise (found the hard way: with a 39-byte base the
   SDK stat'd the socket fine and its HTTP client failed before `connect`, so
   every boot died as "did not create API socket" while a golden's shorter id
   still fit).
5. **Resume derives the VMM uid from the daemon's id**, which is not the id the
   VM layer booted under, so the overlay and snapshot files are chowned to it
   on resume; and in netns mode resume rebuilds a namespace around the baked
   identity instead of falling onto the bridge. Both were silently broken
   before fork's live test exercised pause → fork → resume.

The writable disk is a per-sandbox sparse ext4 overlay sized from the declared
cap. It did not used to be — every sandbox got a hardcoded 5 GB regardless of
what it had been sold — and the security suite is what caught it.

### docker

Production **on hosts without KVM**, and for trusted workloads. Shares the host
kernel; see §Boundaries before putting a hostile tenant on it.

Two limitations that are properties of this adapter rather than of containers:
`multiport` is false because a container publishes 8000 and the template's
declared ports only — those preview correctly on `<port>-<id>.<domain>`, and a
`port` on create, start or `PATCH` that is neither is refused with 400
`port_not_published` rather than 502ing later — and the
writable-disk cap depends on the storage driver. On `--storage-opt size` the
node now **verifies at boot** that its driver actually honours the option, by
writing past a small cap — because a driver that accepts the option and ignores
it produces no error, and that state was previously invisible. A production node
refuses to run unenforced; a dev node warns and exports
`sandboxd_storage_cap_unenforced_total`.

### process

**`-tags dev` builds only. No boundary of any kind** — sandboxes are host
processes in a directory.

**It is not in a released binary.** `nodeboot` reaches it through
`unisolatedRuntime`, which is split on `//go:build dev`; the release half
returns nil and imports nothing, so `internal/runtime/process` is absent from
`go list -deps ./cmd/sandboxd`. That is asserted against the closure, not the
source, by `TestAReleaseBinaryContainsNoUnisolatedRuntime`, with
`TestTheDevBuildDoesContainTheProcessRuntime` as its vacuity guard.

Two gates, not one, because they answer different questions:

| build | `SANDBOXD_DEV` | result |
|---|---|---|
| release (no tag) | unset | refuses, naming `-tags dev` |
| release (no tag) | `1` | **still refuses** — the setting is inert; there is no unisolated runtime linked in to opt in to |
| `-tags dev` | unset | refuses, naming `SANDBOXD_DEV=1` |
| `-tags dev` | `1` | starts on the process runtime |

All four measured on the reference host, running each binary in a container with
no `/dev/kvm` and no Docker (split-runtime-core §3.6).

The package itself carries no build tag: 22 test files across six packages use
it as their Runtime fixture, and a test-only import never enters a binary's
closure. Tagging the package would have deleted that coverage from the default
`go test ./...` run to buy a property the closure test already holds.

## Isolation levels

The vocabulary the API expresses intent in, so that a caller asks for a boundary
rather than for an implementation. Served by `GET /v1/capabilities` on both
tiers and defined in exactly one place, `runtime.LevelFor`
(`internal/runtime/isolation.go`).

> **This table is checked against the code too.**
> `TestIsolationLevelsMatchTheDoc` (`internal/runtime/leveldoc_test.go`) asserts
> every row against `LevelFor` using each runtime's real `DeclaredCaps()`, and
> that every shipped runtime appears in exactly one row. The `satisfied by`
> cells are runtime names as the code spells them; the caveats live in the prose
> below, where a parser will not trip over them.

<!-- BEGIN LEVELS TABLE -->
| level | means | satisfied by |
|---|---|---|
| `strong` | the guest has its own kernel | firecracker |
| `shared` | a real boundary on a shared kernel | docker |
| `trusted` | no tenant boundary is claimed | process |
<!-- END LEVELS TABLE -->

`process` is `-tags dev` builds only, so a released binary offers `strong` and
`shared` and never `trusted` — asserted against the link closure by
`TestAReleaseBinaryContainsNoUnisolatedRuntime`, not by this table. A gVisor
adapter, when it lands, is `shared`: it interposes its own kernel interface but
the host kernel is still there.

**An unrecognised runtime is reported `shared`, never `strong`.** `LevelFor`
takes a name and the `isolation` bit, and its default for a name not in the
table above is the weaker of the two isolating levels. Under-selling a new
driver costs scheduling efficiency until someone adds it; over-selling one puts
untrusted code on a shared kernel with the API's endorsement. A runtime that
reports `isolation: false` is `trusted` whatever it is called — no name gets out
of that.

**The two scopes.** A node's `/v1/capabilities` reports the one level it
provides; the control plane's reports every level *some node* provides, and
`scope` distinguishes the documents. The capability verbs follow the same rule:
`tag_networks`, `checkpoint`, `named_snapshot` and `standby` are the OR over
counted nodes, because placement routes a create that needs one onto a node that
has it. `auto_pause` is the lone AND — it happens to a sandbox unbidden, so one
node that never pauses makes the promise false. The fleet answer is **capability, not
capacity** (`split-runtime-core` D3): a level offered by a node that is
cordoned, draining, or simply full is still offered by the fleet — cordon means
"place nothing new here", not "this machine stopped having its own kernel". Only
a node that has been removed or has stopped heartbeating past `cluster.NodeTTL`
takes its level with it. `nodes.counted` and `nodes.schedulable` are reported
side by side so the gap is visible.

A request for a level no node in the fleet provides is a **400 naming what is
available**, never a silent downgrade — the same treatment template placement
already gives.

## Adding a runtime

1. Implement `rt.Runtime` and `rt.Instance` (`internal/runtime/types.go`). Do
   **not** widen those interfaces: snapshot, restore, lazy memory and diff
   export do not generalise across runtimes and belong in the optional
   capability interfaces beside them.
2. Add `DeclaredCaps()` so the capabilities are readable without a live host.
3. Add a row to the table above, or `TestRuntimeCapabilitiesMatchTheDoc` fails.
4. Implement `CheckIsolation` — the preflight treats an isolating runtime that
   cannot self-verify as a failure, not as a pass.
5. Run `scripts/security-suite.sh --runtime <name> --strict` on a real host and
   record the result. A runtime with no measured reachability matrix has not
   demonstrated its boundary, whatever its `isolation` flag says.

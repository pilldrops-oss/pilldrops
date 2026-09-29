# Fork

> Copy a running sandbox, memory and disk included, in milliseconds.

A fork is a child of the sandbox you fork (the parent). It gets the disk, **the
memory, and every running process** — and carries on under its own id, URL and
token from that point. It is not a snapshot: nothing is written out and booted
from scratch. The parent's memory image and disk are copy-on-write, so the child
is up in tens of milliseconds and uses close to no extra storage until the two
diverge.

## Fork a sandbox

```bash
pilldrops sandbox fork sb-1789647743835-717dd35a --name child
```

```
  source: fork/sb-1789647743835-717dd35a
    boot: 53ms   (parent paused 132ms)

Connect:
  id        sb-1789647746212-fbb1ba11
  cli       pilldrops sandbox exec sb-1789647746212-fbb1ba11 <command>
  browser   https://.../preview/sb-1789647746212-fbb1ba11/7681/
```

Over the API:

```
POST /v1/sandboxes/{id}/fork
{"name": "child", "timeout": 900, "visibility": "org"}   # all optional
```

## Who owns the copy

The child's `created_by` is the key that forked, not the parent's creator, and
its visibility is **private** — even when the parent is visible to the whole
org. That is the point of forking a shared sandbox: you take a copy of a
machine with somebody else's files and processes in it, and it is yours.

Pass `visibility: "org"` (`pilldrops sandbox fork --shared`) to hand the copy
to the org instead. Forking a sandbox you cannot see is a 404, like every other
operation on it — see "Who can see a sandbox" in `docs/pilldrops-cli.md`.

`201` with the create response shape plus `parent_id`, `parent_state`,
`source` (`fork/<parent>`), `boot_ms` (the child's own boot) and
`parent_paused_ms`. With `--json` the CLI prints that document.

The child is a normal sandbox from then on: it counts against the plan's
concurrency like a create, meters on its own clock, and is destroyed on its own.

## Running processes come with it

A server listening on the parent is already listening on the child, at the
child's own URL, without anything starting up. Same PID, same start time; the
child's uptime carries on from the parent's rather than resetting. Whatever the
parent already did — dependencies installed, a database warm, a dev server
compiled — the child skips.

Measured on the reference KVM host (`nb` template, 512 MiB, 1 vCPU), every
row a live check in `fork-live.py`:

| what | number |
|---|---|
| child boot (memory + state restore, own namespace, new token) | 50–55 ms |
| parent paused, 6 MiB dirtied since its last snapshot | 132 ms |
| parent paused, 80 MiB dirtied (test wrote a 64 MiB blob first) | 570 ms |
| overlay copy, reflink | 2–3 ms |
| fork of a **paused** parent | 55 ms boot, parent stays paused |

The pause is Firecracker writing the pages the parent dirtied since it was
restored — not the guest size. See "How it works".

You can fork a sandbox in any state it can be in:

- A **running** parent pauses for the memory write, then carries on.
- A **paused** parent is copied from its snapshot and stays paused.

The fork itself always comes up running.

## After the fork

The two are independent from the moment of the fork. Writes on one never reach
the other; the child gets its own overlay, copy-on-write against the parent's.
The child inherits the parent's size, environment, start command, idle policy
and egress rules, and comes up in its own network namespace, so the parent's
address is not reachable from it and vice versa.

Established outbound TCP connections in the child are dead on arrival: the
host has no connection state for the new namespace. Anything that reconnects
is fine.

## What to use it for

- **Try something risky.** Fork, break the fork, delete it. The original never moved.
- **Run several attempts at once.** Fork three times from one state and compare.
- **Reproduce a bug.** Fork the sandbox that is misbehaving and dig around in the copy.
- **Hand someone a copy.** Give a reviewer or an agent its own machine that already runs your app.

For a copy you want to keep and create sandboxes from later, use a template.
To stop paying for one without losing it, pause it.

## How it works

Every restore — from a golden template image, from a pause snapshot, or from a
fork — turns on Firecracker's dirty-page tracking, and the memory file it was
restored from becomes the sandbox's **base**. A fork (or a pause) then asks for a
*diff* snapshot: only the pages written since the base go to disk, while the
parent stands still. The parent resumes, and the diff is merged onto a reflink
copy of the base to make the child's full image — which is in turn the child's
base, and the parent's new one.

A full snapshot instead has to fault in every page of a file-backed guest:
2.1 s for 512 MiB on the same host, against 0.2 s to write the bytes. That is
the difference between "fork" and "snapshot then boot".

### Node requirements

- **`FC_NETNS=1`** (the default). On the shared bridge the parent's tap and
  address are live and the child, which carries the same baked identity, has
  nowhere to attach; the node answers `fork requires per-sandbox network
  namespaces`.
- **`FC_MEM_SNAPSHOTS=1`**, the same opt-in as pause/resume.
- **One filesystem** for the kernel, templates, overlays, state and the jailer
  chroot base (`FC_JAILER_DIR`, default `/srv/jailer`): the SDK hard-links
  each of them into every VM's chroot. A reflink-capable one — XFS with
  `reflink=1` or btrfs — makes the overlay and memory copies free; on ext4 they
  are real copies and the numbers above grow with the parent's written data.
- **A short `FC_JAILER_DIR`.** The VM API socket lives at
  `<dir>/firecracker/<id>/root/run/<id>.sock` and a Unix socket path caps at
  107 bytes; the node refuses to start past that rather than fail every boot.

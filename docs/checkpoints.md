# Checkpoints

> Named save points you can roll a sandbox back to, in place.

A checkpoint is a copy of a sandbox at one moment: its disk and, on
Firecracker, its memory and every running process. Restoring one rolls the
sandbox **back in place** — same id, same URL, same token, same address — and
throws away everything written since. It is the undo button for a machine.

A fork makes a *second* sandbox from that same capture ([fork.md](fork.md));
a checkpoint keeps the one you have.

## Save, list, restore, remove

```bash
pilldrops sandbox checkpoint save    sb-1789721106777-8b83b1cc before-upgrade
pilldrops sandbox checkpoint list    sb-1789721106777-8b83b1cc
pilldrops sandbox checkpoint restore sb-1789721106777-8b83b1cc before-upgrade
pilldrops sandbox checkpoint rm      sb-1789721106777-8b83b1cc before-upgrade
```

```
  before-upgrade           memory     513.0 MiB  ready      2026-09-18T08:36:12Z
  (1 of 10)
```

`restore` asks before discarding your writes; `-y` skips the prompt and
`--json` prints the raw document.

Over the API:

```
POST   /v1/sandboxes/{id}/checkpoints           {"name": "before-upgrade"}
GET    /v1/sandboxes/{id}/checkpoints
DELETE /v1/sandboxes/{id}/checkpoints/{name}
POST   /v1/sandboxes/{id}/checkpoints/{name}/restore
```

A name is `^[a-z0-9][a-z0-9-]{0,39}$` — it becomes a directory and, on Docker,
an image tag. Saving over a name you already used replaces it; the old one
stays whole until the new one is complete, so a failed re-save costs nothing.

## The sandbox keeps running while you save

On Firecracker the guest stands still only long enough to write the pages it
dirtied since its last capture and copy its disk image. A counter running in
the guest keeps counting across a save, and a server's PID and start time do
not change.

Measured on the reference KVM host (`base` template, 1 vCPU, 1.5 GiB), every
row from `ckpt2-live.py`:

| what | number |
|---|---|
| first save of a sandbox (full memory image) | 1.7–2.1 s paused, 513 MiB |
| every later save (diff against the previous capture) | 13–97 ms paused, 77 MiB |
| overlay copy inside that window (reflink) | 1–3 ms |
| restore | 73–112 ms |

Every save and every listed checkpoint reports its own window as `paused_ms`,
so the number above is measurable per capture rather than only in a benchmark.

The first save is slow because a file-backed guest with no base has to fault in
every page. After that Firecracker tracks dirty pages and writes only those.
A sandbox that has been paused, forked or checkpointed once is already in the
fast case.

## What a restore brings back

| | Firecracker | Docker |
|---|---|---|
| `snapshot_kind` | `memory` | `disk` |
| disk | rolled back | rolled back |
| RAM, PIDs, open sockets | **kept** — same PID, same `/proc` start time | lost |
| start command | not relaunched: it is still running | relaunched, as on a resume |
| id, URL, token, address | unchanged | unchanged |

On Firecracker the restore is the same mechanism as a resume: the VMM is
stopped keeping the disk, the checkpoint's disk image is copied back over the
live overlay, and the machine is restored from the checkpoint's memory image
into a fresh network namespace around the same baked address. A server that was
listening before the save answers on the same preview URL afterwards, with no
startup.

Restoring the same checkpoint twice works: the state and memory files are read,
never consumed — `sha256sum` of both is unchanged after a restore.

A paused sandbox is woken before it is rolled back, because the engine needs a
machine to replace.

## Limits

- **Ten per sandbox.** The eleventh save is `409 checkpoint_limit`; remove one
  or save over an existing name. Only save points you took count: a named
  snapshot save and a backup take a capture of their own on the same disk, and
  those are neither listed nor capped nor reachable through these verbs. Their
  names (`export-…`, `bkp-…`, `rst-…`) are reserved.
- **They die with the sandbox.** Destroy removes every checkpoint, and a
  checkpoint directory with no sandbox is swept when the node starts.
  Checkpoints that outlive their sandbox, or move between nodes, are a
  different feature (`add-named-snapshots`).
- **One node.** A checkpoint lives on the node the sandbox runs on.
- **A capture that was interrupted** lists with `"status": "building"` and is
  refused by a restore (`409 checkpoint_not_ready`). It still holds disk, so it
  is listed rather than hidden.
- **Firecracker needs `FC_MEM_SNAPSHOTS`**, the same flag pause/resume and fork
  need. Without it, and on the process runtime, every verb answers `501`
  naming the runtime. `GET /v1/capabilities` reports `"checkpoint": true` where
  they work.

## Disk

`size_bytes` is what the checkpoint **occupies**, not its apparent size: the
memory image and the disk image are both mostly holes, and their apparent size
would read as the guest's full RAM every time. On a reflinking filesystem
(XFS, Btrfs) the disk copy is nearly free; on ext4 it is a real copy and the
pause window grows with the overlay's allocated blocks. Ten checkpoints of a
1 GiB sandbox is the worst case the cap is protecting against.

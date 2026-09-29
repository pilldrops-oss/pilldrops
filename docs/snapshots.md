# Named snapshots

> Golden images your org owns: set a machine up once, save it under a name, and
> stamp out sandboxes that wake with the work already done.

A named snapshot is a capture of a running sandbox, published to the fleet's
artifact store under a name the org owns. Creating from it produces a **new**
sandbox — new id, URL, token, address and hostname — that on Firecracker comes
up *inside* the captured moment: its processes are still running, its servers
are still listening, and its start command is never run, because the machine is
already past it.

Three related things, easy to confuse:

| | what it makes | lives as long as |
|---|---|---|
| [checkpoint](checkpoints.md) | the *same* sandbox, rolled back | the sandbox |
| [fork](fork.md) | a second sandbox, from a live parent | the child |
| **snapshot** | any number of new sandboxes, from a name | the org deletes it |

## The golden-image recipe

```bash
# 1. set a machine up however you like
pilldrops sandbox create --template python-3.11       # note the ID it prints
pilldrops sandbox exec $ID "pip install -r requirements.txt"
pilldrops sandbox exec $ID "nohup python3 app.py >/dev/null 2>&1 &"
pilldrops sandbox exec $ID "sync"                     # see the last section

# 2. save it under a name, and wait for the bytes to reach the store
pilldrops snapshot save $ID golden --wait

# 3. stamp out machines that wake with the app already serving
pilldrops sandbox create --from-snapshot golden
```

Step 2 prints, once the bytes are in the store:

```
Saving golden v2 from sb-1789733188414-6ca1c1fd — memory
Ready: golden v2 — 1536.0 MiB, 1 vCPU / 512 MB
```

and step 3:

```
Sandbox created
  ID:       sb-1789733205502-49d8ae59
  Template: snap-37a8eec1-golden
  Status:   running
  Expires:  2026-09-18T12:11:45Z
  idle_timeout: 0 (standby)
  hibernate_timeout: 14400 (disk)
  Boot:     snapshot (woke from a memory image) in 254 ms
  Snapshot: golden (memory)
```

`snapshot list` shows every version, newest first:

```
  NAME                     VER  KIND    STATUS          SIZE  CREATED
  golden                   3    memory  ready       1536.0 MiB  2026-09-18 12:06
  golden                   2    memory  deleted        0.0 MiB  2026-09-18 12:06
  golden                   1    memory  deleted        0.0 MiB  2026-09-18 11:43
```

`snapshot rm golden` deletes every version and its objects; it asks first
unless you pass `-y`.

## Over the API

```
POST   /v1/sandboxes/{id}/snapshot   {"name": "golden"}   -> 202
GET    /v1/snapshots
GET    /v1/snapshots/{name}
DELETE /v1/snapshots/{name}
POST   /v1/sandboxes                 {"snapshot": "golden"}
```

A name is `^[a-z0-9][a-z0-9-]{0,39}$`: it becomes part of a template id, so it
can hold nothing a template id cannot.

## Saving does not stop the sandbox

The guest is paused only for as long as it takes to write its memory and copy
its disk, then it carries on. Measured on the reference node with a 512 MB
guest: **the source's counter ran from 5 to 19 across a save**, and its
listener answered throughout.

The save itself is asynchronous, because the memory file is the size of the
guest's RAM and has to travel from the node to the control plane. `POST` answers
**202** with a build id and `"status": "building"`; the name reports `building`
until every object is stored and `ready` after. `--wait` hides that for a human;
scripts should poll `GET /v1/snapshots/{name}`.

A save is not free of the sandbox's checkpoint budget: it takes a slot while it
runs and gives it back when the control plane has the bytes. A sandbox already
holding ten checkpoints answers `409 checkpoint_limit`.

## Versions, and which one you get

Saving a name again makes a new version. A create always gets the newest
**ready** one, so a version still being written is never handed out. Once the
new version is ready, the older versions' objects are deleted — the row stays,
reading `deleted`, so the history is still legible.

The purge runs *after* the new version is marked ready, deliberately: an older
version stays bootable for the whole publish, so a create during a save never
falls through to nothing.

## The size is fixed, and so is the runtime

Firecracker restores into the shape it snapshotted. A create naming a different
vCPU or memory size is refused rather than quietly given the snapshot's:

```json
{"code": "snapshot_size_fixed",
 "error": "snapshot golden was captured at 1 vCPU and 512 MB of memory and
           restores only into that shape; this request asked for 4 vCPU"}
```

The runtime is pinned the same way, from the row: a memory snapshot is never
scheduled onto a Docker node. A create naming a snapshot with no ready version
is `404 snapshot_not_ready` — the same answer for "never existed", "still
building" and "another org's", so the error cannot be used to enumerate names
you are not allowed to see.

## Docker snapshots are disk only, and say so

| | Firecracker | Docker |
|---|---|---|
| what is captured | memory, machine state and disk | the filesystem |
| how it is stored | `snapfile` + `memfile` + `overlay` + `snapmeta`; memory and overlay packed to their data blocks, the root image shared by digest (*Object storage* in [`deployment.md`](deployment.md)) | `image.tar` |
| what a create gets | processes running, servers listening | a cold boot, start command replayed |
| `snapshot_kind` | `"memory"` | `"disk"` |
| `boot_type` | `"snapshot"` | `"cold"` |

`snapshot_kind` is on every response from the 202 onward. A Docker snapshot is
a committed image and nothing else — there is no memory image, and nothing in
the API pretends otherwise. A Docker-only fleet still gets `snapshot
save/list/rm` and `--from-snapshot`, with disk semantics.

On Firecracker the capability also needs per-sandbox network namespaces
(`FC_NETNS=1`, the default) as well as `FC_MEM_SNAPSHOTS=1`: the captured
machine's tap name and address are baked into its memory image, and only a
private namespace has room to hand a child the same ones. `GET
/v1/capabilities` reports `named_snapshot` on the node and on the fleet, where
it is true if any healthy node has it.

## Placement, and the cold node

A snapshot rides an ordinary template id, `snap-<org>-<name>`, so the scheduler
prefers a node that already holds it and falls back to one that can fetch it.
It also filters on `named_snapshot`: the runtime pin a snapshot create carries
says "firecracker", which on its own promises neither `FC_MEM_SNAPSHOTS` nor
`FC_NETNS`, so without the filter the create can land on a node that answers
501 and the caller cannot pick a different one. The filter applies once some
node advertises the capability — an absent heartbeat field reads as false, and
filtering on that would refuse every snapshot create for the length of a
rolling upgrade. A node that has never seen the snapshot fetches its four
objects **and the rootfs of the template it was captured from** — the overlay is
a delta against that disk, so three objects without it is a machine that cannot
start.

Measured on a node with an empty image directory:

```
fetch_ms 9129, boot_ms 253, boot_type "snapshot"
materialised: state, mem, overlay.ext4, meta.json, rootfs.squashfs
```

`fetch_ms` is absent on a node that already held it, so its presence is the
signal.

## Every child gets its own name

Two sandboxes stamped out of one memory image would otherwise both believe they
are the machine that was captured. Each child is told its own hostname as it
comes up, beside its own address:

```bash
$ pilldrops sandbox exec $A hostname
sb-1789731842206-e8387b22
$ pilldrops sandbox exec $B hostname
sb-1789731842985-e113efe6
```

## What a snapshot does not promise

- **It is not a backup of unsynced writes.** A file written and not `sync`ed
  lives in the guest's page cache. The memory image captures that, so a restored
  machine sees the file — but the published disk does not contain it. If you
  care about the disk half, `sync` before you save.
- **No resizing**, on either runtime.
- **No cross-runtime restore**: a memory image is meaningless on Docker.
- **Deleting a version does not kill machines running from it.** Their files are
  hard-linked into the VM's directory and mmapped; removing the store object
  does not touch them. Nothing more is promised than that.

## Where the bytes live

Each version is one `template_builds` row and its objects in the shared artifact
store, exactly like a template build — same publish path, same block maps, same
node-side cache and hard-link fetch.

`ponytail:` on the filesystem artifact backend every version's memory file sits
on the control plane's own disk, 512 MiB to 4 GiB each. `add-artifact-storage-plane`
§1.3a (the S3 backend) is the fleet upgrade; a single-node control plane works
without it.

# Standalone volumes

Status: approved design, 2026-10-12 (first version). Engine change.

## Problem

A sandbox's only writable disk is its overlay, created and destroyed with it.
There is no disk that outlives a sandbox and can be handed to the next one —
boxd's "disks". No `/v1/volumes` route, no table, and every Firecracker VM gets
exactly two drives (`internal/vm/firecracker.go`, `devices()`).

## Design (first version)

A **volume** is an ext4 image file on one node, owned by an org, attached to at
most one sandbox at a time, at create, at a path the caller picks.

- `POST /v1/volumes {"name", "size_mb"}` → `{"id": "vol-…", …}`;
  `GET /v1/volumes`; `GET /v1/volumes/{id}`; `DELETE /v1/volumes/{id}`
  (refused while attached).
- `POST /v1/sandboxes {"volumes": [{"id": "vol-…", "path": "/data"}]}` attaches
  at boot. Destroying the sandbox detaches; the data stays.
- Size: 256 MB–100 GB, counted against the plan's storage like a sandbox disk.

### How it is attached

- The node creates `<VOLUME_DIR>/<id>.ext4` (sparse, `mkfs.ext4`), the same way
  it creates overlays (`internal/vm/storage.go`).
- At boot the VM gets one extra Firecracker drive per volume after `rootfs` and
  `overlay`, so the guest sees `/dev/vdc`, `/dev/vdd`, … in order.
- The node mounts it through envd right after boot (`mkdir -p <path> && mount
  /dev/vdX <path>`). **No template rebuild**: `overlay-init` is untouched. A
  pause/resume keeps the mount, because it lives in the restored memory.
- A sandbox with volumes cold-boots: a golden snapshot was taken with two drives
  and Firecracker restores exactly the drives a snapshot names.

### What the first version refuses (clear 409s, not silent loss)

- **Fork** of a sandbox with a volume: two VMs writing one ext4 image corrupts
  it. Copy-on-fork is a later option.
- **Move, rehome and backup**: they ship the overlay and memory, not the volume.
  A sandbox with a volume stays on its node.
- **Docker runtime**: `capabilities.volumes` is false and a create naming
  volumes is refused, as egress policy is.
- **Checkpoint restore** works, but rolls back the overlay only, not the volume;
  documented.

### Fleet

A volume lives on one node. Nodes report `volumes` in their heartbeat; the
control plane places a new volume on such a node and forwards it there; the
node records its own `node_id`. A create naming volumes is pinned to that node
with `cluster.Request.Node`: 409 when the volumes are on different nodes, 404
for another org's, 503 when the node is away or full. Delete goes to the
volume's node; list and get read the shared store.

## Store (migration 72)

`volumes (id PK, org_id, name, size_mb, node_id, attached_to, created_at)`.

## Testing

- Store: create, list by org, attach/detach, refuse delete while attached.
- Daemon (fake runtime): create with a volume marks it attached, destroy
  detaches, another org's volume refused, an attached volume refused, fork and
  backup of a sandbox with a volume refused.
- HTTP: routes and create field.
- Live on the test node, 2026-10-12: a 1 GB volume mounted as `/dev/vdc` at
  `/data`; a file written there survived pause/resume, fork and delete-while-
  attached returned 409, and after destroy a new sandbox mounting the volume at
  `/mnt/work` read the same file; delete then removed the image.

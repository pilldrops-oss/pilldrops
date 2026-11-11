#!/bin/bash
# Build a template from Dockerfile into squashfs rootfs.
# Usage: ./scripts/build-template.sh python-3.11
set -euo pipefail

TEMPLATE=${1:?Usage: build-template.sh <template-name>}
TEMPLATE_DIR="templates/${TEMPLATE}"
# SNAPSHOT_DIR lets a side-car bake into its own templates dir, never the standing one.
OUTPUT_DIR="${SNAPSHOT_DIR:-/var/lib/sandbox/templates}/${TEMPLATE}"

if [ ! -f "${TEMPLATE_DIR}/Dockerfile" ]; then
    echo "Error: ${TEMPLATE_DIR}/Dockerfile not found"
    exit 1
fi

echo "=== Building template: ${TEMPLATE} ==="

echo "Building Docker image..."
docker build -t "sandbox-${TEMPLATE}" "${TEMPLATE_DIR}"

# Every template image carries the on-demand screen stack, so PD_DESKTOP=1
# works in any sandbox rather than only in the templates that bake a desktop of
# their own. Added here once instead of in every template's Dockerfile: the
# images do not share a base (each has its own FROM), so a shared layer applied
# after the build is the only single place this fits.
#
# The layer body is read from the file the Docker runtime embeds, so the two
# runtimes cannot install different stacks. Read into a variable first: `set -e`
# then catches a missing file, where a `$(cat)` inside the heredoc would
# silently build an image with no desktop.
#
# MEASURED, not the ~60 MB the proposal estimated. In the squashfs this script
# produces, A/B against PD_NO_DESKTOP=1 on the same template (2026-09-20):
#   base      67 MiB -> 219 MiB   delta 152 MiB
#   desktop  314 MiB -> 381 MiB   delta  68 MiB
# (486 MB is the uncompressed Docker layer; zstd takes it to about a third, and
# less again on a template that already carries X.) The bulk is irreducible if
# the image is to hold a real X server — Xvfb pulls libgl1 for GLX, which pulls
# libgl1-mesa-dri + libllvm15 (159 MB of software-GL), and fluxbox pulls
# libimlib2 with its Ghostscript/poppler/x265 loaders (76 MB).
# USER root: a base image may leave a user that cannot run apt-get.
# PD_NO_DESKTOP=1 skips it for an image apt cannot extend; has_desktop then
# lies for that template, which is what the verify-templates.sh probe catches.
if [ "${PD_NO_DESKTOP:-0}" != 1 ]; then
    echo "Adding the desktop stack..."
    DESKTOP_LAYER=$(cat internal/runtime/docker/desktop-layer.dockerfile)
    if ! docker build -t "sandbox-${TEMPLATE}" - <<DESKTOPEOF
FROM sandbox-${TEMPLATE}
USER root
${DESKTOP_LAYER}
DESKTOPEOF
    then
        echo "WARNING: could not add the desktop stack to ${TEMPLATE}." >&2
        echo "         PD_DESKTOP=1 will not work there; drop it from the catalogue" >&2
        echo "         or rebuild with PD_NO_DESKTOP=1 deliberately." >&2
        exit 1
    fi
fi

echo "Exporting filesystem..."
CONTAINER_ID=$(docker create "sandbox-${TEMPLATE}")
docker export "${CONTAINER_ID}" -o "/tmp/${TEMPLATE}.tar"
docker rm "${CONTAINER_ID}"

TMPDIR=$(mktemp -d)
mkdir -p "${TMPDIR}/rootfs"
tar xf "/tmp/${TEMPLATE}.tar" -C "${TMPDIR}/rootfs"

# EVERY copy into ${TMPDIR}/rootfs below passes --remove-destination, and that is
# a containment control, not a tidiness one. `cp` follows a symlink at the
# DESTINATION and writes through it, and an absolute symlink inside an extracted
# rootfs resolves against the HOST's root, not the rootfs. The openclaw image
# (since removed) shipped /sbin/init -> /lib/systemd/systemd, so `cp … rootfs/sbin/init` aimed at
# the host's own systemd; this script runs as root, and it was only ETXTBSY —
# systemd being PID 1 on that machine — that turned it into a build failure
# instead of a corrupted host (measured 2026-09-21, twice, reproducibly). Any
# path a template image can turn into an absolute symlink is a write primitive
# on the build host. --remove-destination unlinks the symlink first.

# envd, rebuilt from source whenever a toolchain is here.
#
# NOT "build it only if bin/envd is missing", which is what this was: a stale
# bin/envd from an earlier provision run was copied into eighteen freshly baked
# rootfs images on 2026-09-21, so a rebake taken as the way to pick up an envd
# change shipped the old binary and the change looked like it had failed. The Go
# build cache makes a no-op rebuild cost about a second, which is nothing against
# a whole-fleet bake — and against debugging a fix that is not in the image.
# A host with no Go still falls back to a prebuilt binary dropped in bin/.
if command -v go >/dev/null 2>&1; then
    echo "=== building bin/envd from source ==="
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/envd ./cmd/envd/
fi
if [ ! -f "bin/envd" ]; then
    echo "Error: bin/envd missing and no Go toolchain to build it — a rootfs without"
    echo "       envd boots with dead exec/files APIs."
    exit 1
fi
cp --remove-destination bin/envd "${TMPDIR}/rootfs/usr/local/bin/envd"
chmod +x "${TMPDIR}/rootfs/usr/local/bin/envd"

# The customer CLI rides along so guest code can call the platform as its own
# sandbox (make cli-linux; internal/templatebuild does the same on the node).
if [ ! -f "bin/pilldrops-linux" ]; then
    echo "Error: bin/pilldrops-linux not found — run 'make cli-linux' first"
    exit 1
fi
cp --remove-destination bin/pilldrops-linux "${TMPDIR}/rootfs/usr/local/bin/pilldrops"
chmod +x "${TMPDIR}/rootfs/usr/local/bin/pilldrops"

# One copy, embedded by internal/templatebuild for the node's build API and
# read from the same file here, so both build paths boot identically.
cp --remove-destination internal/templatebuild/assets/overlay-init "${TMPDIR}/rootfs/sbin/overlay-init"
chmod +x "${TMPDIR}/rootfs/sbin/overlay-init"

# Bake the image's ENV into the guest: the VM boots via /sbin/init, not Docker,
# so Config.Env is otherwise lost.
mkdir -p "${TMPDIR}/rootfs/etc"
docker image inspect "sandbox-${TEMPLATE}" \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    > "${TMPDIR}/rootfs/etc/sandbox-env" 2>/dev/null || true

# An optional start.sh runs as a child of init so it survives; envd kills
# exec'd commands with their process group.
if [ -f "${TEMPLATE_DIR}/start.sh" ]; then
    cp --remove-destination "${TEMPLATE_DIR}/start.sh" "${TMPDIR}/rootfs/usr/local/bin/start.sh"
    chmod +x "${TMPDIR}/rootfs/usr/local/bin/start.sh"
fi

# PID 1, from the same file internal/templatebuild embeds. The in-tree build
# keeps the one line templatebuild drops: the tagged start.sh line, whose
# marker comment goes with it.
cp --remove-destination internal/templatebuild/assets/init "${TMPDIR}/rootfs/sbin/init"

sed -i.bak -e 's| # __INTREE_ONLY__||' "${TMPDIR}/rootfs/sbin/init"
rm -f "${TMPDIR}/rootfs/sbin/init.bak"
chmod +x "${TMPDIR}/rootfs/sbin/init"

# The squashfs root is read-only at boot, so overlay-init cannot mkdir these itself.
mkdir -p "${TMPDIR}/rootfs/rom" "${TMPDIR}/rootfs/overlay" "${TMPDIR}/rootfs/newroot"

# Build to a temp file and mv: the jailer hardlinks rootfs.squashfs into every
# chroot, and -noappend would truncate the inode under running VMs.
echo "Building squashfs..."
mkdir -p "${OUTPUT_DIR}"
mksquashfs "${TMPDIR}/rootfs" "${OUTPUT_DIR}/rootfs.squashfs.tmp" \
    -noappend -comp zstd -Xcompression-level 3
mv -f "${OUTPUT_DIR}/rootfs.squashfs.tmp" "${OUTPUT_DIR}/rootfs.squashfs"

rm -rf "${TMPDIR}" "/tmp/${TEMPLATE}.tar"

# sandboxd's registry scans <templates>/<id>/template.json, so this alone
# publishes the image.
if [ -f "${TEMPLATE_DIR}/template.json" ]; then
    cp "${TEMPLATE_DIR}/template.json" "${OUTPUT_DIR}/template.json"
fi

echo "=== Template built: ${OUTPUT_DIR}/rootfs.squashfs ==="
ls -lh "${OUTPUT_DIR}/rootfs.squashfs"

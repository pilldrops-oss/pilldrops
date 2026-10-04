#!/usr/bin/env bash
# provision-node.sh — provision a bare KVM host so sandboxd can run real
# Firecracker microVMs. Run as root from a checkout of this repo. Idempotent:
# a re-run resumes rather than duplicating work.
#
# Replaces deploy/firecracker-setup.sh, which was deleted in c93be7a and then
# had to be reconstructed by hand from git history. The pinned versions and
# their checksums live IN THIS FILE for that reason — a pin recorded only in a
# comment somewhere else is a pin that gets lost.
#
# What it does (each step skipped when already done):
#   1. host packages: docker (builds template rootfs), squashfs-tools, iptables
#   2. Go toolchain -> /usr/local/go, so envd/sandboxd and `go test -c`
#      build ON the node; without it every change costs a cross-compile dance
#   3. firecracker + jailer -> /usr/bin
#   4. guest kernel -> KERNEL_PATH (the 5.10 pin is load-bearing, see below)
#   5. microVM networking (bridge fcbr0 + NAT) via scripts/setup-host.sh
#   6. template rootfs images via scripts/build-template.sh
#   7. envd + the guest CLI -> /opt/pilldrops/bin (the docker runtime bind-mounts
#      both into every sandbox) and the systemd units from packaging/
#
# Requires a REAL KVM host — /dev/kvm must exist. A cloud VM without it cannot
# run Firecracker; use the Docker runtime path instead.
#
# Env (defaults shown):
#   FC_VERSION=v1.17.0            firecracker/jailer release (see the pin note below)
#   FC_KERNEL_VERSION=5.10.223    guest kernel (see the pin note below)
#   GO_VERSION=1.25.13            must satisfy go.mod
#   KERNEL_PATH=/var/lib/sandbox/vmlinux
#   SNAPSHOT_DIR=/var/lib/sandbox/templates
#   OVERLAY_DIR=/var/lib/sandbox/overlays
#   TEMPLATES=base                comma list of templates to build; set but
#                                 empty (TEMPLATES=) builds none
#   BUILD_DIR=.                   dir holding scripts/ and templates/
#   CLI_REPO=<BUILD_DIR>/../pilldrops-cli   source for the guest CLI binary
set -euo pipefail

# Failure must be visible in the OUTPUT, not only in the exit status: this is
# usually run through `ssh host '…' | tail`, and a pipeline reports tail's exit
# status. A run failed at the template build on 2026-08-28, the wrapper reported
# success, and the missing template was found much later.
_on_err() {
  local rc=$? line=$1
  echo >&2
  echo "✗✗✗ PROVISION-NODE FAILED (exit $rc) at line $line: ${BASH_COMMAND}" >&2
  echo "✗✗✗ The host is PARTIALLY PROVISIONED. Fix the cause and re-run; it is idempotent." >&2
  exit "$rc"
}
trap '_on_err $LINENO' ERR

# Firecracker. v1.17, not v1.7: v1.7's userfaultfd library rejects the ioctl
# flags a 6.x+ host kernel reports ("Unrecognized ioctl flags"), so lazy memory
# (the Uffd memory backend) cannot run on it. Snapshots are NOT portable across
# this bump: v1.17 cannot load a v1.7 memory snapshot, so a node's paused
# sandboxes and pause archives must be resumed before it is upgraded. Golden
# snapshots rebuild themselves.
FC_VERSION="${FC_VERSION:-v1.17.0}"
FC_KERNEL_VERSION="${FC_KERNEL_VERSION:-5.10.223}"
GO_VERSION="${GO_VERSION:-1.25.13}"
KERNEL_PATH="${KERNEL_PATH:-/var/lib/sandbox/vmlinux}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/var/lib/sandbox/templates}"
OVERLAY_DIR="${OVERLAY_DIR:-/var/lib/sandbox/overlays}"
# `-`, not `:-`: an explicitly empty TEMPLATES means "build none".
TEMPLATES="${TEMPLATES-base}"
BUILD_DIR="${BUILD_DIR:-.}"
ARCH="$(uname -m)"

# ── PINS ──────────────────────────────────────────────────────────────────────
# Guest kernel. THE 5.10 PIN IS LOAD-BEARING, not a "latest is fine" version:
# a 6.1 kernel cold-boots the VM happily and then envd never comes up
# ("envd not ready after 10s"), because the guest contract does not match.
# The object key is pinned rather than discovered from a bucket listing, so a
# mirror cannot change what "latest" means between runs.
KERNEL_URL_BASE="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10"
# Firecracker's CI test-fixture bucket publishes no upstream checksum, so these
# are self-computed and this script is the trust anchor (same posture as
# sandboxd-update.sh's SHA256SUMS). Every fetch fails closed on a mismatch.
# Re-pin by hand if FC_KERNEL_VERSION is bumped.
# shellcheck disable=SC2034  # read indirectly via ${!var}
KERNEL_SHA256_x86_64="22847375721aceea63d934c28f2dfce4670b6f52ec904fae19f5145a970c1e65"
# shellcheck disable=SC2034
KERNEL_SHA256_aarch64="eb5d95ac8a67f7a86acf0cb35625633713ad5170b56de8617808d0e18bb832ec"
# Go tarball checksums as published by go.dev/dl.
# shellcheck disable=SC2034
GO_SHA256_x86_64="39042a078ea9ceebe3ecda4a7188f0f5b96e14a071d27923ba7f40b456e85ae3"
# shellcheck disable=SC2034
GO_SHA256_aarch64="adad240fcb6bd180cf973b4b7c747baf4ec81d08b7d40ca35940ee4531971490"

log() { echo "── $*"; }

# Fetch $1 to $2, verifying sha256 $3. Installs nothing on a mismatch.
fetch_verified() {
  local url=$1 dest=$2 want=$3 got tmp
  tmp="$(mktemp)"
  wget -q "$url" -O "$tmp"
  got="$(sha256sum "$tmp" | cut -d' ' -f1)"
  if [ "$got" != "$want" ]; then
    rm -f "$tmp"
    echo "✗ checksum mismatch for $url: got $got, want $want — refusing to install" >&2
    exit 1
  fi
  mv "$tmp" "$dest"
}

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
[ -e /dev/kvm ] || { echo "✗ /dev/kvm not present — this host cannot run Firecracker."; exit 1; }
BUILD_DIR="$(cd "${BUILD_DIR}" && pwd)"
CLI_REPO="${CLI_REPO:-${BUILD_DIR}/../pilldrops-cli}"
[ -f "${BUILD_DIR}/scripts/build-template.sh" ] || { echo "✗ BUILD_DIR=${BUILD_DIR} is not a checkout of this repo"; exit 1; }

# ── 1. host packages ──────────────────────────────────────────────────────────
log "Installing host packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq docker.io squashfs-tools e2fsprogs iptables wget curl ca-certificates file >/dev/null
systemctl enable --now docker >/dev/null 2>&1 || true

mkdir -p "${SNAPSHOT_DIR}" "${OVERLAY_DIR}" /srv/jailer "$(dirname "${KERNEL_PATH}")"

# ── 2. Go toolchain ───────────────────────────────────────────────────────────
# On the node, not on someone's laptop: internal/vm is //go:build linux and runs
# zero tests on macOS, so live work means `go test -c` here, and the template
# build needs `go build` for envd anyway.
if [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" != "go${GO_VERSION}" ]; then
  log "Installing Go ${GO_VERSION}"
  go_var="GO_SHA256_${ARCH}"; go_want="${!go_var:-}"
  [ -n "$go_want" ] || { echo "✗ no pinned Go checksum for ${ARCH}" >&2; exit 1; }
  case "$ARCH" in
    x86_64)  go_arch=amd64 ;;
    aarch64) go_arch=arm64 ;;
    *) echo "✗ unsupported arch ${ARCH}" >&2; exit 1 ;;
  esac
  go_tgz="$(mktemp)"
  fetch_verified "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" "$go_tgz" "$go_want"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$go_tgz"
  rm -f "$go_tgz"
  printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/go.sh
  chmod 0644 /etc/profile.d/go.sh
fi
export PATH="${PATH}:/usr/local/go/bin"
log "go: $(go version)"

# ── 3. firecracker + jailer ───────────────────────────────────────────────────
# Verified against the release's own per-arch .sha256.txt sidecar.
if ! /usr/bin/firecracker --version 2>/dev/null | grep -q "${FC_VERSION}"; then
  log "Installing firecracker + jailer ${FC_VERSION}"
  tmp="$(mktemp -d)"; ( cd "$tmp"
    base_url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
    fname="firecracker-${FC_VERSION}-${ARCH}.tgz"
    wget -q "${base_url}/${fname}" -O "${fname}"
    wget -q "${base_url}/${fname}.sha256.txt" -O "${fname}.sha256.txt"
    sha256sum -c "${fname}.sha256.txt" || { echo "✗ firecracker tarball checksum mismatch — refusing to install" >&2; exit 1; }
    tar xzf "${fname}"
    install -m 0755 "release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/bin/firecracker
    install -m 0755 "release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}"       /usr/bin/jailer )
  rm -rf "$tmp"
fi
log "firecracker: $(/usr/bin/firecracker --version | head -1)  jailer: $(/usr/bin/jailer --version | head -1)"

# ── 4. guest kernel (uncompressed vmlinux; Firecracker cannot boot a bzImage) ─
if [ ! -f "${KERNEL_PATH}" ]; then
  log "Downloading guest kernel ${FC_KERNEL_VERSION} (${ARCH}) -> ${KERNEL_PATH}"
  k_var="KERNEL_SHA256_${ARCH}"; k_want="${!k_var:-}"
  [ -n "$k_want" ] || { echo "✗ no pinned kernel checksum for ${ARCH}; refusing an unverifiable download" >&2; exit 1; }
  fetch_verified "${KERNEL_URL_BASE}/${ARCH}/vmlinux-${FC_KERNEL_VERSION}" "${KERNEL_PATH}" "$k_want"
fi
# mktemp creates 0600 and the mv carries that mode over. The VMM runs as an
# unprivileged per-sandbox uid and opens this file inside its jailer chroot,
# where it is a hard link the jailer does not chown — so a 0600 kernel is one
# the VMM cannot read, and the boot dies before the guest exists. 0644 and NOT
# chown: that inode is shared by every sandbox on the node, so handing one
# sandbox's uid ownership of it would be a cross-tenant write primitive.
chmod 0644 "${KERNEL_PATH}"
log "kernel: $(ls -lh "${KERNEL_PATH}" | awk '{print $5}') ($(file -b "${KERNEL_PATH}" | cut -c1-40))"

# ── 5. microVM networking ─────────────────────────────────────────────────────
log "Configuring microVM networking (setup-host.sh)"
bash "${BUILD_DIR}/scripts/setup-host.sh"
# fcbr0 is kernel state and is lost on reboot exactly like the iptables rules
# next to it; sandbox-egress-restore.service re-runs this copy at boot.
install -D -m 0755 "${BUILD_DIR}/scripts/setup-host.sh" /opt/pilldrops/bin/setup-host.sh

# ── 5b. container egress must work before we build anything with it ───────────
# Step 5 rewrites the host firewall; step 6 builds rootfs images with Docker,
# which needs container egress for apt. dockerd installs its NAT rules at
# startup and does not reinstate them when something else rebuilds the chains.
# Measured 2026-08-28: containers got an address and no NAT, and it surfaced as
# `Temporary failure resolving deb.debian.org` — an apt error for a firewall
# cause. Verify first and restart only if broken: a restart kills every running
# container, and this script may run on a host already serving sandboxes.
EGRESS_TEST_IMAGE="${EGRESS_TEST_IMAGE:-debian:bookworm-slim}"
container_egress_ok() {
  docker run --rm "${EGRESS_TEST_IMAGE}" sh -c 'getent hosts deb.debian.org >/dev/null' >/dev/null 2>&1
}
log "Verifying container egress after the host firewall change"
docker pull -q "${EGRESS_TEST_IMAGE}" >/dev/null 2>&1 || true
if container_egress_ok; then
  log "container egress: OK"
else
  log "container egress BROKEN — restarting dockerd so it reinstalls its iptables rules"
  systemctl restart docker
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
  container_egress_ok || {
    echo "✗ containers still cannot resolve DNS after restarting dockerd; refusing to build templates." >&2
    iptables -t nat -S POSTROUTING | sed 's/^/    /' >&2
    exit 1
  }
  # The restart must not have cost us the rules step 5 just installed. Checked
  # by property, not by the literal: iptables masks host bits off a prefix, so
  # 192.168.30.0/16 goes in and 192.168.0.0/16 comes out.
  iptables -t nat -S POSTROUTING | grep -q '192\.168\.0\.0/16.*MASQUERADE' || {
    echo "✗ setup-host.sh's rules are gone after the dockerd restart; re-run this script." >&2; exit 1; }
  log "container egress: restored, sandbox rules intact"
fi

# ── 6. template rootfs images ─────────────────────────────────────────────────
# build-template.sh builds envd itself now that Go is here; the guest CLI
# comes from the sibling CLI repo (same source as the Makefile's cli-linux).
cd "${BUILD_DIR}"
if [ ! -f bin/pilldrops-linux ]; then
  [ -d "${CLI_REPO}" ] || {
    echo "✗ bin/pilldrops-linux missing and CLI_REPO=${CLI_REPO} not found." >&2
    echo "  Clone the CLI repo next to this one, set CLI_REPO=, or drop a prebuilt" >&2
    echo "  linux/${ARCH} binary at ${BUILD_DIR}/bin/pilldrops-linux." >&2
    exit 1
  }
  log "Building guest CLI from ${CLI_REPO}"
  mkdir -p bin
  ( cd "${CLI_REPO}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" \
      -o "${BUILD_DIR}/bin/pilldrops-linux" ./cmd/pilldrops )
fi
# Comma-separated list; word splitting on the translated spaces drops empties
# and needs no IFS juggling around the build, which runs its own commands.
for t in $(echo "${TEMPLATES}" | tr ',' ' '); do
  log "Building rootfs for template: $t"
  SNAPSHOT_DIR="${SNAPSHOT_DIR}" bash scripts/build-template.sh "$t"
done

# ── 7. node binaries and systemd units ────────────────────────────────────────
# The docker runtime bind-mounts envd and the guest CLI from /opt/pilldrops/bin
# into every sandbox (internal/runtime/docker/docker.go); without them a docker
# sandbox boots with no exec API and no terminal. envd is rebuilt rather than
# reused, for the stale-binary reason build-template.sh gives.
log "Installing envd and the guest CLI into /opt/pilldrops/bin"
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/envd ./cmd/envd/
install -D -m 0755 bin/envd /opt/pilldrops/bin/envd
install -D -m 0755 bin/pilldrops-linux /opt/pilldrops/bin/pilldrops-linux

# No `|| true` anywhere here: sandboxd.service Requires= the restore unit, and
# a node whose restore unit is missing must fail to start, not start open.
# Installed only when absent: a re-run must not overwrite a unit the operator
# edited. Override settings with a drop-in (systemctl edit sandboxd) instead.
for u in sandboxd.service sandbox-egress-restore.service; do
  if [ -e "/etc/systemd/system/$u" ]; then
    if cmp -s "packaging/$u" "/etc/systemd/system/$u"; then
      log "$u already installed and current"
    else
      log "WARNING: /etc/systemd/system/$u differs from packaging/$u and was left as is,"
      log "         so changes to the packaged unit have NOT reached this node. To adopt them:"
      log "         move your edits into a drop-in (systemctl edit $u), then"
      log "         install -m 0644 packaging/$u /etc/systemd/system/$u && systemctl daemon-reload"
    fi
  else
    log "Installing $u from packaging/"
    install -m 0644 "packaging/$u" "/etc/systemd/system/$u"
  fi
done
systemctl daemon-reload
systemctl enable sandbox-egress-restore.service sandboxd.service
for u in sandbox-egress-restore.service sandboxd.service; do
  [ "$(systemctl is-enabled "$u")" = enabled ] || { echo "✗ $u is not enabled" >&2; exit 1; }
done

echo
log "✓ PROVISION COMPLETE — Firecracker host ready. Run sandboxd with:"
echo "     FIRECRACKER_BIN=/usr/bin/firecracker JAILER_BIN=/usr/bin/jailer"
echo "     KERNEL_PATH=${KERNEL_PATH} SNAPSHOT_DIR=${SNAPSHOT_DIR} OVERLAY_DIR=${OVERLAY_DIR}"
echo "     TEMPLATES=${TEMPLATES}"
echo
log "sandboxd.service is installed and enabled, not started: put the node binary at"
log "      /opt/pilldrops/bin/sandboxd-linux (make sandboxd-linux), set its environment"
log "      (docs/deployment.md), then: systemctl start sandboxd"
log "NOTE: a docker-runtime node on this host additionally needs scripts/setup-docker-egress.sh."

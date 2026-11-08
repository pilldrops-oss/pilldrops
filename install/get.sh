#!/bin/sh
# PillDrops single-box installer. On a fresh Linux machine:
#
#   curl -fsSL https://get.pilldrops.com | sudo sh
#
# It downloads a release, checks every file against the release's SHA256SUMS,
# sets the machine up for Firecracker when /dev/kvm exists (Docker otherwise),
# and starts the server as a systemd service. Running it again upgrades.
#
# Settings, all optional:
#   PILLDROPS_VERSION       release tag, e.g. sandboxd/v1.2.0 (default: latest)
#   PILLDROPS_RUNTIME       firecracker or docker (default: firecracker with KVM)
#   PILLDROPS_PORT          API and dashboard port (default 8080)
#   PILLDROPS_DOMAIN        wildcard domain for app previews, e.g. sandbox.example.com
#   PILLDROPS_RELEASE_BASE  URL of a directory laid out like a release, for
#                           mirrors and testing; it must also hold source.tar.gz
set -eu

REPO=pilldrops-oss/pilldrops
BIN=/opt/pilldrops/bin
SRC=/opt/pilldrops/src
STATE=/var/lib/sandbox/state
ENV_FILE=/etc/pilldrops/sandboxd.env
PORT="${PILLDROPS_PORT:-8080}"

say() { printf '── %s\n' "$*"; }
die() { printf '✗ %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root: curl -fsSL https://get.pilldrops.com | sudo sh"
[ "$(uname -s)" = Linux ] || die "the server runs on Linux; on macOS or Windows install only the CLI (npm install -g @pilldrops/cli)"
[ "$(uname -m)" = x86_64 ] || die "releases are built for x86_64 only, this machine is $(uname -m)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

if ! command -v curl >/dev/null 2>&1 || ! command -v sha256sum >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die "install curl and coreutils, then run this again"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates coreutils tar >/dev/null
fi

base="${PILLDROPS_RELEASE_BASE:-}"
if [ -z "$base" ]; then
  tag="${PILLDROPS_VERSION:-}"
  if [ -z "$tag" ]; then
    tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
      sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$tag" ] || die "could not find the latest release of $REPO"
  fi
  base="https://github.com/$REPO/releases/download/$tag"
  source_url="https://github.com/$REPO/archive/refs/tags/$tag.tar.gz"
else
  tag="${PILLDROPS_VERSION:-from $base}"
  source_url="$base/source.tar.gz"
fi
say "installing PillDrops $tag"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
files="sandboxd-linux-amd64 envd-linux-amd64 pilldrops-linux-amd64"
curl -fsSL "$base/SHA256SUMS" -o "$stage/SHA256SUMS" || die "no SHA256SUMS at $base"
for f in $files; do
  curl -fsSL "$base/$f" -o "$stage/$f" || die "could not download $f"
  grep " $f\$" "$stage/SHA256SUMS" >/dev/null || die "$f is not listed in SHA256SUMS"
done
(cd "$stage" && grep -E " ($(echo "$files" | tr ' ' '|'))\$" SHA256SUMS | sha256sum -c --quiet -) ||
  die "a download does not match SHA256SUMS; nothing was installed"
say "downloads match SHA256SUMS (check the signature with scripts/verify-release.sh before trusting a release)"

# The provisioning scripts and templates come from the same release's source.
curl -fsSL "$source_url" -o "$stage/source.tar.gz" || die "could not download the source for $tag"
rm -rf "$SRC" && mkdir -p "$SRC" "$SRC/bin"
tar -xzf "$stage/source.tar.gz" -C "$SRC" --strip-components=1
install -m 0755 "$stage/pilldrops-linux-amd64" "$SRC/bin/pilldrops-linux"

runtime="${PILLDROPS_RUNTIME:-}"
if [ -z "$runtime" ]; then
  if [ -e /dev/kvm ]; then runtime=firecracker; else runtime=docker; fi
fi
case "$runtime" in
  firecracker)
    [ -e /dev/kvm ] || die "PILLDROPS_RUNTIME=firecracker needs /dev/kvm"
    say "KVM found: setting up microVMs (kernel, networking, base template)"
    (cd "$SRC" && BUILD_DIR="$SRC" TEMPLATES=base bash scripts/provision-node.sh) ||
      die "provisioning failed; the lines above say where"
    ;;
  docker)
    say "using Docker containers, a weaker boundary than a microVM (see SECURITY.md)"
    if ! command -v docker >/dev/null 2>&1; then
      command -v apt-get >/dev/null 2>&1 || die "install Docker from your distribution, then run this again"
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io >/dev/null
      systemctl enable --now docker >/dev/null
    fi
    bash "$SRC/scripts/setup-docker-egress.sh" || die "Docker egress rules could not be applied"
    for u in sandboxd.service sandbox-egress-restore.service; do
      install -m 0644 "$SRC/packaging/$u" "/etc/systemd/system/$u"
    done
    ;;
  *) die "PILLDROPS_RUNTIME must be firecracker or docker, not $runtime" ;;
esac

mkdir -p "$BIN" "$(dirname "$ENV_FILE")"
systemctl stop sandboxd 2>/dev/null || true
install -m 0755 "$stage/sandboxd-linux-amd64" "$BIN/sandboxd-linux"
install -m 0755 "$stage/envd-linux-amd64" "$BIN/envd"
install -m 0755 "$stage/pilldrops-linux-amd64" "$BIN/pilldrops-linux"
install -m 0755 "$stage/pilldrops-linux-amd64" /usr/local/bin/pilldrops

# Kept across upgrades: it seals stored sandbox env, so a new one would lose it.
secret=$(sed -n 's/^SANDBOXD_SECRET_KEY=//p' "$ENV_FILE" 2>/dev/null || true)
[ -n "$secret" ] || secret=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
umask 077
{
  echo "PORT=$PORT"
  echo "SANDBOXD_DATA=$STATE"
  echo "SANDBOXD_RUNTIME=$runtime"
  echo "SANDBOXD_SECRET_KEY=$secret"
  echo "TEMPLATE=base"
  echo "SANDBOXD_SANDBOX_DOMAIN=${PILLDROPS_DOMAIN:-}"
} > "$ENV_FILE"
umask 022
mkdir -p /etc/systemd/system/sandboxd.service.d
cat > /etc/systemd/system/sandboxd.service.d/single-box.conf <<UNIT
# Written by get.sh. The packaged unit is for a fleet node; these settings make
# it the single-box server: its own API and dashboard, state in SQLite.
[Service]
EnvironmentFile=$ENV_FILE
UNIT
mkdir -p "$STATE" && chmod 0700 "$STATE"
systemctl daemon-reload
systemctl enable sandbox-egress-restore.service sandboxd.service >/dev/null 2>&1
systemctl restart sandbox-egress-restore.service
systemctl restart sandboxd

say "waiting for the server"
i=0
until curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:$PORT/health"; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    journalctl -u sandboxd -n 25 --no-pager >&2
    if journalctl -u sandboxd -n 50 --no-pager | grep -q "disk caps"; then
      die "Docker on this disk cannot cap each sandbox's storage. Use a machine with KVM, or put /var/lib/docker on XFS mounted with pquota and set {\"storage-driver\":\"overlay2\",\"features\":{\"containerd-snapshotter\":false}} in /etc/docker/daemon.json, then run this again"
    fi
    die "the server did not start; its log is above"
  fi
  sleep 2
done

host=$(hostname -I 2>/dev/null | awk '{print $1}')
if [ "$runtime" = docker ]; then isolation="Docker containers"; else isolation=microVMs; fi
cat <<DONE

✓ PillDrops is running (sandboxes are $isolation).

  Dashboard   http://${host:-<this-host>}:$PORT/dashboard   (the first visit creates the admin account)
  API key     sudo cat $STATE/bootstrap.key                  (store it, then delete the file)
  CLI         pilldrops login --api-url http://${host:-<this-host>}:$PORT --key <key>

  Put HTTPS in front before exposing it to the internet, and read SECURITY.md.
  Upgrade: run the same command again.
DONE

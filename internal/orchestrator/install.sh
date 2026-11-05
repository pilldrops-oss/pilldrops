#!/usr/bin/env sh
# PillDrops node bootstrap. Run on a fresh host:
#   curl -fsSL <orch>/install.sh -o sf.sh && ORCHESTRATOR_URL=<orch> sh sf.sh
# and paste the join token when prompted. Non-interactive callers set
# JOIN_TOKEN_FILE=/path (preferred) or JOIN_TOKEN. The token is never part of
# the command line so it stays out of shell history and /proc/<pid>/cmdline.
# Re-running re-registers the node.
set -eu
: "${ORCHESTRATOR_URL:?set ORCHESTRATOR_URL}"

# The fetched binaries run as root, so the transport is the trust anchor:
# refuse plaintext except loopback (local `make dev`).
case "$ORCHESTRATOR_URL" in
  https://*) ;;
  http://127.0.0.1[:/]*|http://localhost[:/]*|http://127.0.0.1|http://localhost)
    echo "warning: plaintext ORCHESTRATOR_URL allowed for loopback only" >&2 ;;
  *)
    echo "refusing to bootstrap over a plaintext ORCHESTRATOR_URL: ${ORCHESTRATOR_URL}" >&2
    echo "this script fetches binaries and runs them as root; use https://." >&2
    exit 2 ;;
esac

# Resolve the token without putting it on a command line.
if [ -n "${JOIN_TOKEN_FILE:-}" ]; then
  JOIN_TOKEN=$(cat "$JOIN_TOKEN_FILE")
elif [ -z "${JOIN_TOKEN:-}" ]; then
  # Read from /dev/tty, not stdin, which may be the piped script itself.
  if [ -r /dev/tty ]; then
    printf 'Join token: ' > /dev/tty
    stty -echo < /dev/tty 2>/dev/null || true
    read -r JOIN_TOKEN < /dev/tty
    stty echo < /dev/tty 2>/dev/null || true
    printf '\n' > /dev/tty
  fi
fi
[ -n "${JOIN_TOKEN:-}" ] || { echo "no join token supplied (prompt, JOIN_TOKEN_FILE, or JOIN_TOKEN)" >&2; exit 2; }
NODE_ZONE="${NODE_ZONE:-par1}"
BIN_DIR=/opt/pilldrops/bin
PUBLIC_IP="${PUBLIC_IP:-$(curl -fsSL https://api.ipify.org || echo 127.0.0.1)}"
ADVERTISE_URL="${ADVERTISE_URL:-http://${PUBLIC_IP}:18080}"

if [ -e /dev/kvm ] && { command -v firecracker >/dev/null 2>&1 || [ -x /usr/bin/firecracker ]; }; then
  echo "firecracker-capable host (/dev/kvm + firecracker present) — skipping Docker install"
elif ! command -v docker >/dev/null 2>&1; then
  # Distro package, not `curl get.docker.com | sh`: apt verifies signatures
  # against a trust root this script did not fetch.
  echo "installing docker (fallback runtime) from the distro package…"
  if command -v apt-get >/dev/null 2>&1; then
    sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io
    sudo systemctl enable --now docker >/dev/null 2>&1 || true
  else
    echo "no apt-get on this host and no docker present." >&2
    echo "install docker from your distribution's signed package repository, then re-run." >&2
    echo "(this installer will not pipe a remote script into a root shell.)" >&2
    exit 2
  fi
fi
sudo mkdir -p "$BIN_DIR"

# The orchestrator serves NODE_BIN_DIR under /bin/<name>; the join binary is served
# as pilldrops-join-linux but installed as pilldrops-join. A missing manifest is a
# refusal, not a fallback: an optional integrity check is one an attacker omits.
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

echo "fetching the binary manifest…"
if ! curl -fsSL --connect-timeout 10 --max-time 60 "${ORCHESTRATOR_URL}/bin/SHA256SUMS" -o "${STAGE}/SHA256SUMS"; then
  echo "refusing to install unverified binaries: ${ORCHESTRATOR_URL}/bin/SHA256SUMS is not published." >&2
  echo "publish it alongside the binaries in NODE_BIN_DIR:" >&2
  echo "  (cd \"\$NODE_BIN_DIR\" && sha256sum sandboxd-linux envd pilldrops-join-linux > SHA256SUMS)" >&2
  exit 2
fi

stage() { curl -fsSL --connect-timeout 10 --max-time 600 "${ORCHESTRATOR_URL}/bin/$1" -o "${STAGE}/$1"; }
stage sandboxd-linux
stage envd
stage pilldrops-join-linux

# --ignore-missing lets the manifest cover more than is fetched here; every
# fetched file must still match.
echo "verifying binaries against the manifest…"
if ! (cd "$STAGE" && sha256sum -c --ignore-missing SHA256SUMS >/dev/null 2>&1); then
  echo "✗ binary verification FAILED against ${ORCHESTRATOR_URL}/bin/SHA256SUMS — nothing installed." >&2
  (cd "$STAGE" && sha256sum -c --ignore-missing SHA256SUMS 2>&1 | grep -v ': OK$' >&2) || true
  exit 1
fi
# A manifest that lists none of the downloads would verify vacuously.
for f in sandboxd-linux envd pilldrops-join-linux; do
  if ! grep -q "  ${f}\$" "${STAGE}/SHA256SUMS"; then
    echo "✗ ${f} is absent from SHA256SUMS — it would have been installed unverified." >&2
    exit 1
  fi
done
echo "binaries verified."

install_bin() { sudo install -m 0755 -o root -g root "${STAGE}/$1" "${BIN_DIR}/$2"; }
install_bin sandboxd-linux sandboxd-linux
install_bin envd envd
install_bin pilldrops-join-linux pilldrops-join

# The token goes in on stdin; printf is a builtin, so no cmdline ever carries it.
printf '%s' "$JOIN_TOKEN" | sudo env ORCHESTRATOR_URL="$ORCHESTRATOR_URL" \
  NODE_ZONE="$NODE_ZONE" ADVERTISE_URL="$ADVERTISE_URL" \
  "${BIN_DIR}/pilldrops-join"
unset JOIN_TOKEN

# Must stay in step with packaging/sandboxd.service (overwrite is fine on re-onboard).
sudo tee /etc/systemd/system/sandboxd.service >/dev/null <<UNIT
[Unit]
Description=PillDrops node agent
After=network-online.target docker.service sandbox-egress-restore.service
# Requires=, not Wants=: a node whose egress rules failed to restore must not
# start serving. See packaging/sandbox-egress-restore.service.
Requires=sandbox-egress-restore.service
[Service]
EnvironmentFile=/etc/sandboxd.env
ExecStart=${BIN_DIR}/sandboxd-linux
Restart=on-failure
RestartSec=3
# Turns the fail-closed guards ON: ProductionDBError, IsolationGuardError,
# StorageCapGuardError, SnapshotGuardError. Without it every one is inert.
Environment=SANDBOXD_PRODUCTION=1
MemoryMax=60%
TasksMax=8192
NoNewPrivileges=yes
[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable sandboxd
# restart, not enable --now: a re-run must replace a running node's identity.
sudo systemctl restart sandboxd

# systemctl returns once the unit is queued, so ask the node itself.
tries=0
until curl -fsS --max-time 2 -o /dev/null http://127.0.0.1:18080/health; do
  tries=$((tries + 1))
  if [ "$tries" -ge 60 ]; then
    echo "sandforge: the node did not come up. Its log:" >&2
    sudo journalctl -u sandboxd -n 20 --no-pager >&2 || true
    exit 1
  fi
  sleep 2
done
echo "sandforge: node started — check the fleet in the orchestrator console."

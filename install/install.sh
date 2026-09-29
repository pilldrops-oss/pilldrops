#!/bin/sh
# sandboxd installer: builds (or copies) the binary, installs a systemd unit, starts it.
# Usage: curl -fsSL <url>/install.sh | sh   (run from a checkout, or with BIN set)
set -eu

PREFIX="${PREFIX:-/usr/local/bin}"
DATA="${SANDBOXD_DATA:-/var/lib/sandboxd}"

echo "── Installing sandboxd to ${PREFIX}/sandboxd"
if [ -n "${BIN:-}" ] && [ -f "${BIN}" ]; then
  install -m 0755 "${BIN}" "${PREFIX}/sandboxd"
elif [ -f "./bin/sandboxd" ]; then
  install -m 0755 ./bin/sandboxd "${PREFIX}/sandboxd"
elif command -v go >/dev/null 2>&1 && [ -d ./cmd/sandboxd ]; then
  echo "── Building from source with go…"
  go build -o "${PREFIX}/sandboxd" ./cmd/sandboxd/
else
  echo "error: no prebuilt ./bin/sandboxd, no BIN=..., and no Go toolchain to build from source" >&2
  exit 1
fi

mkdir -p "${DATA}"

if command -v systemctl >/dev/null 2>&1 && [ -d /etc/systemd/system ]; then
  echo "── Installing systemd unit"
  cp packaging/sandboxd.service /etc/systemd/system/sandboxd.service 2>/dev/null || true
  systemctl daemon-reload
  systemctl enable --now sandboxd 2>/dev/null || true
  echo "── sandboxd started. First-run API key:"
  journalctl -u sandboxd -n 20 --no-pager 2>/dev/null | grep -A1 'first-run API key' || true
else
  echo "── No systemd; start manually:  SANDBOXD_DATA=${DATA} ${PREFIX}/sandboxd"
fi
echo "── Done. Health: curl localhost:8080/health"

#!/bin/sh
# PillDrops CLI installer, served by every PillDrops server:
#
#   curl -fsSL http://<your server>/install.sh | sh
#
# Installs the `pilldrops` binary from the latest release of
# github.com/pilldrops-oss/pilldrops-cli (or from $PILLDROPS_BASE_URL/dl/ when
# set) into /usr/local/bin, falling back to ~/.local/bin when that isn't writable.
set -eu

RELEASES="https://github.com/pilldrops-oss/pilldrops-cli/releases/latest/download"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$OS" in
  darwin|linux) ;;
  *) echo "install.sh: unsupported OS: $OS (darwin and linux binaries are published)" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "install.sh: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

DEST="/usr/local/bin"
if [ ! -d "$DEST" ] || [ ! -w "$DEST" ]; then
  DEST="$HOME/.local/bin"
  mkdir -p "$DEST"
fi

URL="$RELEASES/pilldrops_${OS}_${ARCH}"
if [ -n "${PILLDROPS_BASE_URL:-}" ]; then
  URL="$PILLDROPS_BASE_URL/dl/pilldrops_${OS}_${ARCH}"
fi
echo "→ downloading pilldrops ($OS/$ARCH) from $URL …"
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
curl -fSL --progress-bar "$URL" -o "$TMP"
chmod +x "$TMP"
mv "$TMP" "$DEST/pilldrops"
trap - EXIT

echo "✓ installed $DEST/pilldrops"
case ":$PATH:" in
  *":$DEST:"*) ;;
  *) echo "⚠ $DEST is not on your PATH — add it to your shell profile" ;;
esac
echo ""
echo "next:"
echo "  pilldrops login --api-url <your server> --key <key>"
echo "  pilldrops sandbox create --template python-3.11"
echo "  pilldrops ai                       # AI agents: the full usage catalog"

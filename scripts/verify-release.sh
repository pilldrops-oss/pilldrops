#!/usr/bin/env bash
# verify-release.sh: check that a downloaded sandboxd release was signed by this
# repository's release workflow. The signature is verified first; SHA256SUMS
# alone proves nothing.
#
# Usage:
#   ./scripts/verify-release.sh <dir-with-downloaded-assets> [repo]
#
# Needs cosign (https://docs.sigstore.dev/cosign/installation/).
set -euo pipefail

dir="${1:?usage: verify-release.sh <dir> [owner/repo]}"
repo="${2:-pilldrops-oss/pilldrops}"

command -v cosign >/dev/null || { echo "cosign not installed" >&2; exit 1; }
cd "$dir"

for f in SHA256SUMS SHA256SUMS.sig; do
  [ -f "$f" ] || { echo "missing $f — download it from the release" >&2; exit 1; }
done

echo "==> verifying the signature over SHA256SUMS"
if [ -f SHA256SUMS.pem ]; then
  # Signed by the release workflow. Without the identity constraints cosign
  # accepts any Sigstore identity.
  cosign verify-blob \
    --certificate SHA256SUMS.pem \
    --signature SHA256SUMS.sig \
    --certificate-identity-regexp "^https://github.com/${repo}/\.github/workflows/release\.yml@refs/tags/sandboxd/" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    SHA256SUMS
else
  # Signed by scripts/release-local.sh with the key whose public half is
  # release.pub. Take it from a checkout you trust, not from the release.
  key="${RELEASE_PUB:-$(git -C "$OLDPWD" rev-parse --show-toplevel 2>/dev/null)/release.pub}"
  [ -f "$key" ] || { echo "no SHA256SUMS.pem, so this release is key-signed; set RELEASE_PUB to release.pub" >&2; exit 1; }
  cosign verify-blob --insecure-ignore-tlog=true --key "$key" --signature SHA256SUMS.sig SHA256SUMS
fi

echo "==> verifying the binaries against the signed checksums"
# Only check files actually present, so one downloaded binary verifies alone.
present=$(awk '{print $2}' SHA256SUMS | while read -r f; do [ -f "$f" ] && echo "$f"; done)
[ -n "$present" ] || { echo "none of the checksummed files are here" >&2; exit 1; }
grep -F -f <(echo "$present") SHA256SUMS | sha256sum -c -

echo
echo "OK: the signature holds and the bytes match."

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

for f in SHA256SUMS SHA256SUMS.sig SHA256SUMS.pem; do
  [ -f "$f" ] || { echo "missing $f — download it from the release" >&2; exit 1; }
done

# Without the identity constraints cosign accepts any Sigstore identity.
echo "==> verifying the signature over SHA256SUMS"
cosign verify-blob \
  --certificate SHA256SUMS.pem \
  --signature SHA256SUMS.sig \
  --certificate-identity-regexp "^https://github.com/${repo}/\.github/workflows/release\.yml@refs/tags/sandboxd/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS

echo "==> verifying the binaries against the signed checksums"
# Only check files actually present, so one downloaded binary verifies alone.
present=$(awk '{print $2}' SHA256SUMS | while read -r f; do [ -f "$f" ] && echo "$f"; done)
[ -n "$present" ] || { echo "none of the checksummed files are here" >&2; exit 1; }
grep -F -f <(echo "$present") SHA256SUMS | sha256sum -c -

echo
echo "OK: signed by ${repo}'s release workflow, and the bytes match."

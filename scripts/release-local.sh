#!/usr/bin/env bash
# release-local.sh: build, check and sign a release on this machine, for when
# the release workflow cannot run. Produces the same assets as
# .github/workflows/release.yml, signed with a cosign key instead of the
# workflow's identity.
#
# Usage:
#   COSIGN_KEY=cosign.key scripts/release-local.sh sandboxd/v1.2.0 [--publish]
#
# Without --publish it writes dist/release/ and stops. With it, it creates the
# GitHub release. Run from a clean checkout of main with the tag at HEAD.
#
# Env:
#   COSIGN_KEY  private key from `cosign generate-key-pair`; its public half is
#               release.pub at the repository root. Required to publish.
#   CLI_REPO    pilldrops-cli checkout the CLI is built from (default ../pilldrops-cli)
set -euo pipefail

tag="${1:?usage: release-local.sh sandboxd/vX.Y.Z [--publish]}"
publish="${2:-}"
case "$tag" in sandboxd/v*) ;; *) echo "the tag must look like sandboxd/vX.Y.Z" >&2; exit 2 ;; esac
cd "$(git rev-parse --show-toplevel)"
CLI_REPO="${CLI_REPO:-../pilldrops-cli}"

[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "the checkout has uncommitted changes" >&2; exit 1; }
[ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] || { echo "$tag is not at HEAD" >&2; exit 1; }
git fetch --quiet --no-tags origin main
git merge-base --is-ancestor HEAD origin/main || { echo "$tag is not on main" >&2; exit 1; }
[ -d "$CLI_REPO" ] || { echo "CLI_REPO=$CLI_REPO not found" >&2; exit 1; }
if [ "$publish" = --publish ]; then
  [ -n "${COSIGN_KEY:-}" ] || { echo "publishing needs COSIGN_KEY: an unsigned release proves nothing about who built it" >&2; exit 1; }
  [ -f release.pub ] || { echo "release.pub is not committed, so nobody could check the signature" >&2; exit 1; }
fi

version="${tag#sandboxd/}"
out=dist/release
rm -rf "$out" && mkdir -p "$out"

echo "==> dashboard, tests and tier guard, as the release workflow runs them"
make web
go test -race -timeout 30m ./...
./scripts/tier-boundary-guard.sh

echo "==> binaries"
ldflags="-s -w -X github.com/pilldrops-oss/pilldrops/internal/nodeboot.Version=${version}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$ldflags" -o "$out/sandboxd-linux-amd64" ./cmd/sandboxd/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$ldflags" -o "$out/sandboxd-fleet-linux-amd64" ./cmd/sandboxd-fleet/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$out/envd-linux-amd64" ./cmd/envd/
cli_rev=$(git -C "$CLI_REPO" rev-parse --short HEAD)
(cd "$CLI_REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" \
  -o "$OLDPWD/$out/pilldrops-linux-amd64" ./cmd/pilldrops)
(cd "$out" && if command -v sha256sum >/dev/null; then sha256sum -- *-linux-amd64; else shasum -a 256 -- *-linux-amd64; fi > SHA256SUMS)

if [ -n "${COSIGN_KEY:-}" ]; then
  echo "==> signing SHA256SUMS"
  # No transparency-log upload: the key in release.pub is the trust root here.
  cosign sign-blob --yes --use-signing-config=false --new-bundle-format=false --tlog-upload=false --key "$COSIGN_KEY" --output-signature "$out/SHA256SUMS.sig" "$out/SHA256SUMS"
  cosign verify-blob --insecure-ignore-tlog=true --key release.pub --signature "$out/SHA256SUMS.sig" "$out/SHA256SUMS"
fi
if command -v syft >/dev/null; then
  syft dir:. -o spdx-json="$out/sbom.spdx.json" --quiet
fi
ls -l "$out"

[ "$publish" = --publish ] || { echo; echo "built in $out; run again with --publish to create the release"; exit 0; }
assets=("$out"/*-linux-amd64 "$out/SHA256SUMS" "$out/SHA256SUMS.sig")
[ -f "$out/sbom.spdx.json" ] && assets+=("$out/sbom.spdx.json")
git push origin "$tag"
gh release create "$tag" "${assets[@]}" --repo pilldrops-oss/pilldrops --title "sandboxd ${version}" --notes \
  "sandboxd ${version}. Install or upgrade: \`curl -fsSL https://get.pilldrops.com | sudo sh\`. Verify first with \`scripts/verify-release.sh\` and release.pub. Built and signed outside GitHub Actions; CLI from pilldrops-cli ${cli_rev}."

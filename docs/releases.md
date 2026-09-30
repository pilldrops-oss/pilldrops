# Releases: what is published, and how to check it

You are about to run a binary that executes untrusted code on your hardware.
This page is how you satisfy yourself it is the binary this repository built.

## What a release contains

| asset | what it is |
|---|---|
| `sandboxd-fleet-linux-amd64` | the node agent — what you run on a fleet node |
| `sandboxd-linux-amd64` | the full standalone node (node + API + dashboard on one box) |
| `envd-linux-amd64` | the in-guest agent. Shipped alongside so the pair stays in sync — a node and a guest agent from different releases is a supported-but-untested combination |
| `SHA256SUMS` | hashes of the three binaries |
| `SHA256SUMS.sig`, `SHA256SUMS.pem` | Sigstore signature and certificate over `SHA256SUMS` |
| `sbom.spdx.json` | SPDX SBOM of the source tree the binaries were built from |

## Cutting a release

Releases come only from tags on `main`; a push to a branch builds and tests but
publishes nothing.

```bash
git switch main && git pull
git tag sandboxd/v1.0.0
git push origin sandboxd/v1.0.0
```

The release workflow refuses a tag whose commit is not on `main`, builds the
dashboard from the tagged source, runs the tests, then builds, signs and
publishes the binaries above. The dashboard is not committed; every binary
embeds the one built for its tag.

## Verify

```bash
./scripts/verify-release.sh ~/Downloads/sandboxd-release
```

Or by hand:

```bash
cosign verify-blob \
  --certificate SHA256SUMS.pem \
  --signature   SHA256SUMS.sig \
  --certificate-identity-regexp '^https://github.com/pilldrops-oss/pilldrops/\.github/workflows/release\.yml@refs/tags/sandboxd/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS

sha256sum -c SHA256SUMS
```

**The order is the point.** Verify the signature *first*, then the hashes it
vouches for. A checksum file on its own proves nothing an attacker could not also
produce: whoever can replace a binary in a release can replace `SHA256SUMS`
beside it. What they cannot do is mint a Sigstore certificate for this
repository's release workflow.

**The identity flags are not optional.** Without
`--certificate-identity-regexp` and `--certificate-oidc-issuer`, `cosign` will
verify that *somebody* signed the file and tell you nothing about who. That is
the most common way this check gets run and gets nothing.

## On a node: `sandboxd-update` verifies for you

`sandboxd-update` runs the same check before it replaces the running binary, and
**refuses the update if it does not pass** — including when `cosign` is not
installed. There is no `--skip-verify`. Install cosign on every node:

```bash
# https://docs.sigstore.dev/cosign/installation/
cosign version
```

The expected signer identity is pinned *in the updater on the node*, not
downloaded with the release. That is deliberate: a key or an identity fetched
from the same place as the artifact is the same circularity as the checksum, one
level up. Forks that sign under a different workflow set
`SANDBOXD_SIGNER_IDENTITY_RE` / `SANDBOXD_SIGNER_ISSUER` in
`/etc/sandboxd-update.env` (root-owned, 0600).

The `sandboxd-update` tool and the test that exercised this refusal end-to-end
(valid checksums plus an attacker-supplied signature must not install) were part
of the node-update kit deleted with `deploy/` in `c93be7a`. `scripts/verify-release.sh`
is the half that survived, and it is the one you run by hand.

## What the signature does and does not tell you

It says: these bytes were produced by `release.yml` in this repository, at a tag
matching `sandboxd/v*`, and have not changed since.

It does not say the code is safe, that the workflow was not compromised at the
time it ran, or that the tag was pushed by someone you trust. Signing binds an
artifact to a build; it does not audit the build. If you need more than that,
build from source — the release job is `go build` with `CGO_ENABLED=0` and no
network access beyond the checked-in `vendor/` tree, so you can reproduce it.

## Why keyless

There is no signing key in this repository and none in its secrets. `cosign`
exchanges GitHub's OIDC token for a short-lived certificate at build time. A key
that does not exist cannot leak, cannot need rotating, and cannot be used by
anyone who gains access to the repository's secrets later.

The trade is that verification depends on Sigstore's public infrastructure being
reachable, and that the certificate is only meaningful together with the identity
flags above.

## Dependencies

`vendor/` is checked in, so the source tree already answers *which versions are
in this binary* exactly — the build cannot resolve a different one. The SBOM
exists because that answer is not otherwise legible to someone who has not
cloned the repo, and because vulnerability tooling expects the format.

CI runs `govulncheck` on every push, which reports only vulnerabilities that are
actually *reachable* from this code rather than every advisory that touches a
vendored module.

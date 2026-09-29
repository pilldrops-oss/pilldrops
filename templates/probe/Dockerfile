# The security suite's probe image.
#
# Not a user-facing template. It exists because scripts/security-suite.sh cannot
# honestly run against `base`, and the reason is narrower than "base is minimal":
#
#   base HAS  sh bash curl git openssl timeout getent  — and bash's /dev/tcp
#             works, so outbound TCP probes are perfectly possible from it.
#   base LACKS ping (no ICMP), and nc/socat/python3 — so it cannot LISTEN.
#
# A sandbox that cannot listen cannot be the peer in a sandbox-to-sandbox test,
# and a probe that connects to a peer with nothing bound reports "refused"
# whether or not the network is isolated. That is a vacuous pass, and it is the
# failure this image removes: measured 2026-08-28, a VM-to-VM probe run from
# base "passed" while proving nothing, because the listener it was talking to
# had never started (python3 was absent) and the ICMP leg sent no packets (ping
# was absent).
#
# Kept deliberately small and pinned to the same base as `base`, so a probe
# result reflects the platform rather than a different distribution.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl ca-certificates \
    iproute2 iputils-ping netcat-openbsd socat \
    procps psmisc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

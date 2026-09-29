# Hermes Agent.
#
# PROVENANCE — read before changing this file.
#
# There is no official npm distribution. `hermes` on npm is segmentio's unrelated
# "Messenger of the gods"; the package used here, `hermes-agent`, describes itself as
# an "Unofficial npm bridge for Hermes Agent" and is maintained at
# github.com/wyrtensi/hermes-agent-npm. Its postinstall fetches the agent's source
# over git, so what lands in this image comes from a third party's repackaging.
#
# This was a deliberate decision (2026-08-09) to unblock parity, taken knowing the
# agent runs with the customer's own provider keys. Replace this with the official
# distribution when one exists.
#
# Node 24: Hermes' bridge installs cleanly here. Its postinstall REQUIRES git — without
# it the install fails outright (verified), which is the good failure mode.
FROM node:24-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pinned. An unpinned install is how a template silently changes agent underneath a
# customer — and for OpenClaw an unpinned install resolved to a 0.0.1 stub with no
# binary at all, so pinning here is a lesson already learned once.
RUN npm install -g hermes-agent@0.20.0

# Fail the BUILD if the agent is not actually runnable. Without this the image ships
# happily and every task fails at runtime with no stated reason — which reads as our
# bug rather than a broken install.
RUN command -v hermes >/dev/null || (echo "hermes binary missing after install" && exit 1)

WORKDIR /root

# Provider keys (HERMES_* or the provider's own) are supplied at sandbox-create time
# via env. The envd/agentd binaries and overlay-init are injected by build-template.sh.

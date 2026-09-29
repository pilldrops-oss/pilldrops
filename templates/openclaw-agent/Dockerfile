# OpenClaw — the real agent (github.com/openclaw/openclaw), a multi-channel AI gateway.
#
# Not to be confused with the `openclaw` template id, which is a browser-automation
# Python sandbox that borrowed the name and contains no OpenClaw. That id is left
# alone deliberately: a sandbox re-boots from its stored template id, so repointing it
# would silently change what an existing workstation runs.
#
# NODE 24 IS LOAD-BEARING. OpenClaw requires Node >=22.22.3 <23, >=24.15.0 <25, or
# >=25.9.0. On node:20 a global install of openclaw does NOT fail — it walks backwards to
# openclaw@0.0.1, a stub with no binary at all, and the image ships looking healthy
# while every task fails for no stated reason. Verified 2026-08-09.
FROM node:24-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Pinned to the version that was actually verified. `latest` moved during this work
# (2026.7.1-2 stable, 2026.7.2-beta.7 published), and an unpinned install is how a
# template changes agent underneath a customer.
RUN npm install -g openclaw@2026.7.1-2

# Fail the BUILD if the agent is not runnable — this is the exact failure the Node
# version note above describes, caught here instead of at runtime.
RUN command -v openclaw >/dev/null || (echo "openclaw binary missing after install" && exit 1)

WORKDIR /root

# Provider keys are supplied at sandbox-create time via env; `openclaw agent --local`
# reads them from the shell. envd/agentd and overlay-init are injected by
# build-template.sh.

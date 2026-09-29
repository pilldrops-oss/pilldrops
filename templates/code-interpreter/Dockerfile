FROM python:3.11-slim-bookworm

# System packages: build toolchain + the shell tools agents assume exist
# (procps = ps/free/pkill/uptime; psmisc = killall; zip/unzip; jq). Installed at
# BUILD time on the node host (full RAM), so this never hits the 1 GB-VM
# runtime-apt OOM path.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git build-essential procps psmisc zip unzip jq xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node LTS via the official static tarball into /usr/local (bin/node, bin/npm,
# bin/npx, lib/node_modules). Debian's apt nodejs is 18 and the apt npm chain
# OOMs the runtime VM; the tarball ships a current LTS (>= 20.19, newer than
# E2B's default v20.9) with zero apt cost.
ARG NODE_VERSION=20.19.0
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) nodearch="x64" ;; \
      arm64) nodearch="arm64" ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${nodearch}.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 --no-same-owner \
        --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE'; \
    node --version; npm --version

# Pre-install common Python data-science packages to avoid cold-start penalty.
RUN pip install --no-cache-dir \
    numpy pandas matplotlib scipy scikit-learn jupyter requests pytest

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

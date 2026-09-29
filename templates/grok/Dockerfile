FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# NOTE: confirm the canonical Grok CLI package at build time.
RUN npm install -g @vibe-kit/grok-cli@0.0.34

WORKDIR /root

# Agent API keys (e.g. XAI_API_KEY) are supplied at sandbox-create time via env.
# The envd binary and overlay-init are injected by build-template.sh

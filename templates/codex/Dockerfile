FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @openai/codex@0.147.0

WORKDIR /root

# Agent API keys (e.g. OPENAI_API_KEY) are supplied at sandbox-create time via env.
# The envd binary and overlay-init are injected by build-template.sh

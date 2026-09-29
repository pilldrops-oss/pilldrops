FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @sourcegraph/amp@0.0.1786119357-g68379f

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

FROM node:20-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g typescript tsx ts-node

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

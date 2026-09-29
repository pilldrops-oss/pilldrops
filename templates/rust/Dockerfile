FROM rust:1.77-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git build-essential pkg-config libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

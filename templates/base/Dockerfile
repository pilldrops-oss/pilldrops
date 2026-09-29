FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

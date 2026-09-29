FROM python:3.11-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git build-essential chromium \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
    playwright selenium beautifulsoup4 requests flask

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

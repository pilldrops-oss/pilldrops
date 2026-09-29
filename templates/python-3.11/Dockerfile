FROM python:3.11-slim-bookworm

# System packages for common data science / web dev use cases
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    git \
    build-essential \
    && rm -rf /var/lib/apt/lists/*

# Pre-install common packages to avoid cold-start penalty
RUN pip install --no-cache-dir \
    requests \
    numpy \
    pandas

# Working directory
WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

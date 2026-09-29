# Security review template. Every tool here inspects code and configuration the
# caller already owns — dependency manifests, git history, and the response
# headers of a URL. There is deliberately NO active vulnerability scanner or
# payload fuzzer: pointing one of those at a third party from shared
# infrastructure is an attack, not a feature.
FROM python:3.11-slim-bookworm

ARG OSV_VERSION=2.4.0
ARG GITLEAKS_VERSION=8.30.1
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && ARCH="${TARGETARCH:-amd64}" \
    # osv-scanner: known-vulnerable dependencies, from Google's OSV database.
    && curl -fsSL -o /usr/local/bin/osv-scanner \
       "https://github.com/google/osv-scanner/releases/download/v${OSV_VERSION}/osv-scanner_linux_${ARCH}" \
    && chmod +x /usr/local/bin/osv-scanner \
    # gitleaks: secrets committed to the repo or its history.
    && case "$ARCH" in amd64) GL_ARCH=x64 ;; arm64) GL_ARCH=arm64 ;; *) GL_ARCH="$ARCH" ;; esac \
    && curl -fsSL "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${GL_ARCH}.tar.gz" \
       | tar xz -C /usr/local/bin gitleaks \
    && chmod +x /usr/local/bin/gitleaks \
    && osv-scanner --version && gitleaks version

COPY pdscan /usr/local/bin/pdscan
COPY report.py /usr/local/bin/pd-report
# Installed BOTH as pd-start (the documented manual entry point) and as
# /usr/local/bin/start.sh — the path every runtime's init/startIfPresent hook
# looks for. With only pd-start, the report server never auto-started on
# docker nodes (build-template.sh's separate start.sh copy hid this on
# firecracker).
COPY start.sh /usr/local/bin/pd-start
COPY start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/pdscan /usr/local/bin/pd-report /usr/local/bin/pd-start /usr/local/bin/start.sh \
    && mkdir -p /root/security

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

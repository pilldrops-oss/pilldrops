# python base: the report server + renderer are python, and this shares layers
# with the fastapi / code-interpreter images already on every node.
FROM python:3.11-slim-bookworm

# k6 (pinned) for load generation; curl/git for fetching scripts. TARGETARCH
# keeps the image buildable on amd64 nodes and arm64 dev machines alike.
ARG K6_VERSION=0.54.0
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && ARCH="${TARGETARCH:-amd64}" \
    && curl -fsSL "https://github.com/grafana/k6/releases/download/v${K6_VERSION}/k6-v${K6_VERSION}-linux-${ARCH}.tar.gz" \
       | tar xz -C /tmp \
    && mv "/tmp/k6-v${K6_VERSION}-linux-${ARCH}/k6" /usr/local/bin/k6 \
    && rm -rf /tmp/k6-* \
    && k6 version

COPY pdload /usr/local/bin/pdload
COPY report.py /usr/local/bin/pd-report
# Both names: pd-start is the documented manual entry point; start.sh is the
# path every runtime's init/startIfPresent hook auto-runs (see the note in
# templates/security/Dockerfile).
COPY start.sh /usr/local/bin/pd-start
COPY start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/pdload /usr/local/bin/pd-report /usr/local/bin/pd-start /usr/local/bin/start.sh \
    && mkdir -p /root/loadtest

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

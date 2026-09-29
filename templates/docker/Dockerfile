FROM ubuntu:24.04

# Docker's own apt repo: the distro's docker.io lags and ships no compose/buildx
# plugin. iptables comes from the distro.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg iptables uidmap mount \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io \
        docker-buildx-plugin docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# The guest kernel is 5.10 with CONFIG_NF_TABLES=n (probe, tasks.md §Probe
# result). Ubuntu 24.04 points iptables at the nft backend, which cannot load
# there, so dockerd would start and then fail to program NAT — every -p publish
# silently dead. CONFIG_IP_NF_IPTABLES=y, so the legacy backend is the right
# answer rather than a workaround.
RUN update-alternatives --set iptables /usr/sbin/iptables-legacy \
    && update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

ENV DOCKER_HOST=unix:///var/run/docker.sock

WORKDIR /root

# The envd binary and overlay-init are injected by build-template.sh

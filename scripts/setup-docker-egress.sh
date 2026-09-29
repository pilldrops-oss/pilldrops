#!/usr/bin/env bash
# setup-docker-egress.sh: DOCKER-USER egress DROP for cloud metadata and internal
# services. Idempotent. Requires Docker running; installs iptables if absent.
set -euo pipefail

if ! command -v iptables >/dev/null 2>&1; then
  apt-get update -qq && apt-get install -y -qq iptables
fi

iptables -N DOCKER-USER 2>/dev/null || true  # no-op if dockerd already created it

# -I inserts at position 1, so the last -I ends up on top. DROPs first, ESTABLISHED,RELATED last.
iptables -C DOCKER-USER -d 100.64.0.0/10  -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 100.64.0.0/10  -j DROP  # CGNAT
iptables -C DOCKER-USER -d 172.16.0.0/12  -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 172.16.0.0/12  -j DROP  # RFC1918 (covers docker0 172.17/16)
iptables -C DOCKER-USER -d 172.17.0.1/32  -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 172.17.0.1/32  -j DROP  # docker gateway -> host (explicit)
iptables -C DOCKER-USER -d 10.0.0.0/8     -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 10.0.0.0/8     -j DROP  # RFC1918
iptables -C DOCKER-USER -d 169.254.0.0/16 -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 169.254.0.0/16 -j DROP  # metadata (all providers) + link-local
iptables -C DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
iptables -I DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

# IPv6 backstop. The runtime disables IPv6 in new containers, but containers
# adopted across an upgrade still have a link-local stack that bypasses every
# IPv4 rule above. Guarded because `set -e` would abort on a host without ip6tables.
if command -v ip6tables >/dev/null 2>&1 && ip6tables -L >/dev/null 2>&1; then
  ip6tables -N DOCKER-USER 2>/dev/null || true
  ip6tables -C DOCKER-USER -j DROP 2>/dev/null || ip6tables -I DOCKER-USER -j DROP
  # DOCKER-USER is reached from FORWARD only; container->host goes through INPUT.
  ip6tables -C INPUT -s fe80::/10 -j DROP 2>/dev/null || ip6tables -I INPUT -s fe80::/10 -j DROP
  echo "IPv6: DOCKER-USER DROP + link-local INPUT DROP applied (backstop for pre-upgrade containers)."
else
  echo "IPv6: no ip6tables on this host; skipping (nothing to filter)."
fi

echo "Docker egress rules applied: metadata/RFC1918/CGNAT/gateway DROP on DOCKER-USER."

# Sandbox -> node. A packet to the host's own bridge address traverses INPUT,
# never DOCKER-USER, so the DROPs above do not cover it and the node API binds
# 0.0.0.0. The subnet is read from Docker because dockerd allocates it per host.
SB_NET="$(docker network inspect sandboxd-net -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true)"
if [ -z "$SB_NET" ]; then
  # sandboxd creates sandboxd-net at boot, so a first run before it has started finds nothing.
  echo "WARN: sandboxd-net does not exist yet; sandbox->host INPUT rules NOT applied." >&2
  echo "      Re-run this script after sandboxd has started once." >&2
else
  # -I inserts at the top: DROP first, then the ESTABLISHED,RELATED ACCEPT so it
  # lands above. Without it replies to node-opened connections (preview proxy) are dropped.
  iptables -C INPUT -s "$SB_NET" -j DROP 2>/dev/null || \
  iptables -I INPUT -s "$SB_NET" -j DROP
  iptables -C INPUT -s "$SB_NET" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
  iptables -I INPUT -s "$SB_NET" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  echo "Sandbox->host INPUT DROP applied for $SB_NET (replies to node-initiated connections still allowed)."
fi

# sandbox-egress-restore.service restores these files before sandboxd starts.
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4
# Only when there is something to save, so the restore unit never sees an empty rules.v6.
if command -v ip6tables-save >/dev/null 2>&1 && ip6tables -L >/dev/null 2>&1; then
  ip6tables-save > /etc/iptables/rules.v6
fi

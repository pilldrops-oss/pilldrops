#!/usr/bin/env bash
# setup-docker-egress.sh: DOCKER-USER egress rules for docker sandboxes, matching
# what setup-host.sh gives Firecracker VMs: no metadata, RFC1918 or host, and
# only DNS/HTTP/HTTPS out. Idempotent. Requires Docker running; installs
# iptables if absent.
set -euo pipefail

if ! command -v iptables >/dev/null 2>&1; then
  apt-get update -qq && apt-get install -y -qq iptables
fi

iptables -N DOCKER-USER 2>/dev/null || true  # no-op if dockerd already created it

# sandboxd creates sandboxd-net at boot with these options; creating it here
# means a first run, before sandboxd has started, still gets the rules keyed
# on its subnet instead of skipping them.
docker network inspect sandboxd-net >/dev/null 2>&1 || \
  docker network create -o com.docker.network.bridge.enable_icc=false sandboxd-net >/dev/null
SB_NET="$(docker network inspect sandboxd-net -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}')"
[ -n "$SB_NET" ] || { echo "FAIL: cannot read the sandboxd-net subnet" >&2; exit 1; }

# Delete then insert, so a re-run restores the order below rather than
# stacking new rules above old ones. -I prepends: the last call ends on top.
top() { # top CHAIN RULE...
  local chain=$1; shift
  while iptables -D "$chain" "$@" 2>/dev/null; do :; done
  iptables -I "$chain" "$@"
}

# The allowlist setup-host.sh gives Firecracker VMs: DNS, HTTP, HTTPS. Without
# it a sandbox reached the orchestrator, Postgres and MinIO on peer hosts
# through this node's address, which their firewalls trust. RETURN, not
# ACCEPT, so docker's own chains still see the packet.
top DOCKER-USER -s "$SB_NET" -j DROP
top DOCKER-USER -s "$SB_NET" -p tcp --dport 443 -j RETURN
top DOCKER-USER -s "$SB_NET" -p tcp --dport 80 -j RETURN
top DOCKER-USER -s "$SB_NET" -p tcp --dport 53 -j RETURN
top DOCKER-USER -s "$SB_NET" -p udp --dport 53 -j RETURN
# Destination DROPs above the allowlist, which is destination-blind.
top DOCKER-USER -d 100.64.0.0/10 -j DROP                 # CGNAT
top DOCKER-USER -s "$SB_NET" -d 192.168.0.0/16 -j DROP   # RFC1918; scoped so other docker networks here keep working
top DOCKER-USER -d 172.16.0.0/12 -j DROP                 # RFC1918 (covers docker0 172.17/16)
top DOCKER-USER -d 172.17.0.1/32 -j DROP                 # docker gateway -> host (explicit)
top DOCKER-USER -d 10.0.0.0/8 -j DROP                    # RFC1918
top DOCKER-USER -d 169.254.0.0/16 -j DROP                # metadata (all providers) + link-local
top DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

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

echo "Docker egress rules applied: metadata/RFC1918/CGNAT/gateway DROP and the DNS/HTTP/HTTPS allowlist for $SB_NET on DOCKER-USER."

# Sandbox -> node. A packet to the host's own addresses traverses INPUT, never
# DOCKER-USER, so the DROPs above do not cover it and the node API binds
# 0.0.0.0. DROP first, then the ESTABLISHED,RELATED ACCEPT so it lands above;
# delete-then-insert like DOCKER-USER, so a partial earlier run cannot leave the
# DROP above it. Without it replies to node-opened connections (preview proxy)
# are dropped.
top INPUT -s "$SB_NET" -j DROP
top INPUT -s "$SB_NET" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
echo "Sandbox->host INPUT DROP applied for $SB_NET (replies to node-initiated connections still allowed)."

# sandbox-egress-restore.service restores these files before sandboxd starts.
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4
# Only when there is something to save, so the restore unit never sees an empty rules.v6.
if command -v ip6tables-save >/dev/null 2>&1 && ip6tables -L >/dev/null 2>&1; then
  ip6tables-save > /etc/iptables/rules.v6
fi

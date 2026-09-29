#!/bin/bash
# Firecracker host setup: bridge, NAT and egress rules. Idempotent; run as root.
set -euo pipefail

echo "=== Sandbox host setup ==="

mkdir -p /var/lib/sandbox/{templates,overlays}
mkdir -p /srv/jailer

ip link add fcbr0 type bridge 2>/dev/null || true
ip addr add 192.168.30.1/16 dev fcbr0 2>/dev/null || true
ip link set fcbr0 up

sysctl -w net.ipv4.ip_forward=1
grep -q "ip_forward" /etc/sysctl.conf || echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf

iptables -t nat -C POSTROUTING -s 192.168.30.0/16 -j MASQUERADE 2>/dev/null || \
iptables -t nat -A POSTROUTING -s 192.168.30.0/16 -j MASQUERADE

# -I prepends, so the ESTABLISHED,RELATED ACCEPT inserted last sits above the
# DROP; without it the host cannot reach envd. Delete both first so a re-run
# cannot stack a DROP above the ACCEPT.
while iptables -D INPUT -s 192.168.30.0/16 -j DROP 2>/dev/null; do :; done
while iptables -D INPUT -s 192.168.30.0/16 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null; do :; done
iptables -I INPUT  -s 192.168.30.0/16 -j DROP              # VMs can't open new conns to host
iptables -I INPUT  -s 192.168.30.0/16 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT  # but allow replies to host-initiated conns
iptables -C FORWARD -s 192.168.30.0/16 -d 169.254.0.0/16 -j DROP 2>/dev/null || \
iptables -I FORWARD -s 192.168.30.0/16 -d 169.254.0.0/16 -j DROP  # no metadata/link-local (provider-agnostic: covers Scaleway 169.254.42.42, AWS/GCP 169.254.169.254)
iptables -C FORWARD -s 192.168.30.0/16 -d 192.168.30.0/16 -j DROP 2>/dev/null || \
iptables -I FORWARD -s 192.168.30.0/16 -d 192.168.30.0/16 -j DROP  # no VM-to-VM

# Replies to VM-initiated connections; the allowlist below only matches outbound.
iptables -C FORWARD -d 192.168.30.0/16 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
iptables -I FORWARD -d 192.168.30.0/16 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

# RFC1918 + CGNAT DROP: the 53/80/443 allowlist is destination-blind. 192.168/16
# is the VM bridge, covered by the VM-to-VM DROP above. -I, not -A, so on a
# re-run these land above the already-present allowlist ACCEPTs.
for cidr in 10.0.0.0/8 172.16.0.0/12 100.64.0.0/10; do
  iptables -C FORWARD -s 192.168.30.0/16 -d "$cidr" -j DROP 2>/dev/null || \
  iptables -I FORWARD -s 192.168.30.0/16 -d "$cidr" -j DROP
done

# Allowlist: DNS, HTTP, HTTPS only. Guarded so a re-run does not append duplicates.
add_forward() {
  iptables -C FORWARD "$@" 2>/dev/null || iptables -A FORWARD "$@"
}
add_forward -s 192.168.30.0/16 -p udp --dport 53  -j ACCEPT
add_forward -s 192.168.30.0/16 -p tcp --dport 53  -j ACCEPT
add_forward -s 192.168.30.0/16 -p tcp --dport 80  -j ACCEPT
add_forward -s 192.168.30.0/16 -p tcp --dport 443 -j ACCEPT
add_forward -s 192.168.30.0/16 -j DROP              # block all else

echo "Network rules applied."
echo "VMs: DNS + HTTP + HTTPS only. VM-to-VM: blocked. Host: blocked."

# sandbox-egress-restore.service restores this file before sandboxd starts.
mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4

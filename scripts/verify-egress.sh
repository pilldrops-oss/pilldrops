#!/usr/bin/env bash
# verify-egress.sh: asserts the egress-isolation DROP rules are present and
# reachable. A DROP below an ACCEPT that matches the same packet is dead, so
# every rule is checked for its position in the chain, not just its existence.
# Used by the node preflight and by ops after a reboot.
#
# Usage: verify-egress.sh [--firecracker|--docker|--netns]  (default: --firecracker)
# Exit:  0 if all expected rules are present and ordered; non-zero with a FAIL
#        message on stderr otherwise.
#
# --netns verifies only the host half of a netns-mode node (the sandboxd_netns
# nft table and ip_forward); the per-sandbox policy lives inside each namespace
# and is probed by the boot-time canary. The --firecracker host rules are
# bypassed by design on such a node.
set -euo pipefail
mode="${1:---firecracker}"
fail() { echo "FAIL: $1" >&2; exit 1; }

# line_of <chain-dump> <extended-regex> prints the 1-based position of the first
# matching rule, or "" if none. Under `set -e` a bare `$(… | grep …)` aborts before
# `fail` can name the rule, and `head -1` can SIGPIPE the producer under pipefail.
line_of() {
  local m
  m=$(printf '%s\n' "$1" | grep -nE -- "$2") || m=""
  m=${m%%$'\n'*}         # first match only
  printf '%s' "${m%%:*}" # its leading line number; "" when there was no match
}

case "$mode" in
  --firecracker)
    # Read the chain once so ordering compares positions from one snapshot.
    fwd="$(iptables -S FORWARD)"
    # Installed as 192.168.30.0/16, but iptables masks host bits and prints
    # 192.168.0.0/16; grepping the unmasked literal never matches.
    vm='192\.168\.0\.0/16'
    # Ordering reference: the first ACCEPT of a VM-sourced packet (setup-host.sh
    # appends a 53/80/443 allowlist), matched on `-s` so the `-d` reply-ACCEPT, which
    # cannot short-circuit outbound packets, is not it. No allowlist means no DROP is shadowed.
    allow_at=$(line_of "$fwd" "-s $vm .* -j ACCEPT")
    # $1 = escaped destination CIDR, $2 = human label.
    forward_drop() {
      local at
      at=$(line_of "$fwd" "-s $vm -d $1 -j DROP")
      [ -n "$at" ] || fail "FC $2 FORWARD DROP missing (run setup-host.sh)"
      [ -z "$allow_at" ] || [ "$at" -lt "$allow_at" ] \
        || fail "the FC $2 FORWARD DROP is BELOW the VM allowlist ACCEPT (line $at vs $allow_at) — it never matches, so sandbox egress to that range is NOT blocked"
    }
    # All five families setup-host.sh installs.
    forward_drop '169\.254\.0\.0/16' 'metadata/link-local 169.254.0.0/16'
    forward_drop '10\.0\.0\.0/8'     'RFC1918 10.0.0.0/8'
    forward_drop '172\.16\.0\.0/12'  'RFC1918 172.16.0.0/12'
    forward_drop '100\.64\.0\.0/10'  'CGNAT 100.64.0.0/10'
    forward_drop "$vm"               'VM-to-VM 192.168.0.0/16'
    # VM -> node: the INPUT DROP and the reply-ACCEPT that must sit above it.
    inp="$(iptables -S INPUT)"
    fc_re="-s $vm"
    fc_drop=$(line_of "$inp" "$fc_re -j DROP")
    fc_accept=$(line_of "$inp" "$fc_re .*RELATED,ESTABLISHED -j ACCEPT")
    [ -n "$fc_drop" ] || fail "FC VM->host INPUT DROP missing (run setup-host.sh)"
    [ -n "$fc_accept" ] || fail "FC VM->host ESTABLISHED,RELATED ACCEPT missing — the node cannot reach envd, so every sandbox fails to become ready"
    [ "$fc_accept" -lt "$fc_drop" ] || fail "the FC ESTABLISHED,RELATED ACCEPT is BELOW the DROP (line $fc_accept vs $fc_drop) — it never matches, so every create fails with 'envd not ready'" ;;
  --docker)
    # DOCKER-USER is reached only via the `-j DOCKER-USER` jump in FORWARD; an
    # ACCEPT above that jump bypasses every rule below.
    dfwd="$(iptables -S FORWARD)"
    du_at=$(line_of "$dfwd" '-j DOCKER-USER')
    [ -n "$du_at" ] || fail "FORWARD has no -j DOCKER-USER jump — none of the container egress DROPs below it are reachable"
    # ESTABLISHED ACCEPTs cannot match a new outbound packet, so they are not a
    # bypass. Walk the full chain so $n is in the same coordinates as $du_at.
    du_accept_at=""
    n=0
    while IFS= read -r line; do
      n=$((n + 1))
      case "$line" in *"-j ACCEPT"*) ;; *) continue ;; esac
      if printf '%s' "$line" | grep -qE -- '--ctstate [A-Z,]*ESTABLISHED'; then continue; fi
      du_accept_at=$n
      break
    done <<<"$dfwd"
    [ -z "$du_accept_at" ] || [ "$du_at" -lt "$du_accept_at" ] \
      || fail "the FORWARD -j DOCKER-USER jump is BELOW an ACCEPT (line $du_at vs $du_accept_at) — container egress can bypass every DOCKER-USER DROP"
    du="$(iptables -S DOCKER-USER)"
    # The sandboxd-net subnet is allocated by dockerd from its address pools,
    # which an operator may move to 10/8 or 192.168/16, so it is read, not guessed.
    sb_net=$(docker network inspect sandboxd-net -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null) || sb_net=""
    [ -n "$sb_net" ] || fail "cannot read the sandboxd-net subnet from docker — the network is missing or docker is down (run setup-docker-egress.sh)"
    sb_re="-s ${sb_net//./\\.}"
    # The DNS/HTTP/HTTPS allowlist setup-host.sh gives Firecracker VMs. Without
    # its closing DROP a sandbox reaches every port on every public address,
    # the control plane on a peer host included.
    [ -n "$(line_of "$du" "$sb_re -j DROP")" ] \
      || fail "Docker sandbox port allowlist missing — sandboxes reach any port on any public address, including peer hosts' control plane (run setup-docker-egress.sh)"
    # The allowlist is destination-blind, so every DROP must sit above it.
    ret_at=$(line_of "$du" "$sb_re .*-j RETURN")
    du_drop() {
      local at
      at=$(line_of "$du" "$1 -j DROP")
      [ -n "$at" ] || fail "Docker $2 DROP missing (run setup-docker-egress.sh)"
      [ -z "$ret_at" ] || [ "$at" -lt "$ret_at" ] \
        || fail "the Docker $2 DROP is BELOW the sandbox allowlist (line $at vs $ret_at) — sandbox traffic to that range on 53/80/443 is NOT blocked"
    }
    du_drop '-d 169\.254\.0\.0/16'          'metadata 169.254.0.0/16'
    du_drop '-d 172\.17\.0\.1/32'           'gateway 172.17.0.1'
    du_drop '-d 10\.0\.0\.0/8'              'RFC1918 10/8'
    du_drop '-d 172\.16\.0\.0/12'           'RFC1918 172.16/12'
    du_drop "$sb_re -d 192\.168\.0\.0/16"   'RFC1918 192.168/16'
    du_drop '-d 100\.64\.0\.0/10'           'CGNAT 100.64/10'
    # sandbox -> node is delivered locally via INPUT, not DOCKER-USER.
    dinp="$(iptables -S INPUT)"
    drop_at=$(line_of "$dinp" "$sb_re -j DROP")
    # Replies on node-opened connections need this ACCEPT above the DROP.
    # iptables prints the state as RELATED,ESTABLISHED whatever order was installed.
    accept_at=$(line_of "$dinp" "$sb_re .*RELATED,ESTABLISHED -j ACCEPT")
    [ -n "$drop_at" ] || fail "Docker sandbox->host INPUT DROP missing (run setup-docker-egress.sh)"
    [ -n "$accept_at" ] || fail "Docker sandbox->host ESTABLISHED,RELATED ACCEPT missing — the node cannot reach its own sandboxes"
    [ "$accept_at" -lt "$drop_at" ] || fail "the ESTABLISHED,RELATED ACCEPT is BELOW the DROP (line $accept_at vs $drop_at) — it never matches" ;;
  --netns)
    # With ip_forward 0 every guest packet dies silently. `sysctl -n` rather
    # than /proc so tests can stub it from PATH.
    ipf=$(sysctl -n net.ipv4.ip_forward 2>/dev/null) \
      || fail "cannot read net.ipv4.ip_forward"
    [ "$ipf" = "1" ] || fail "net.ipv4.ip_forward is ${ipf:-unreadable} — netns sandbox egress is silently dropped in the host namespace"
    tbl=$(nft list table ip sandboxd_netns 2>&1) \
      || fail "nft table sandboxd_netns missing — the node's host-side netns rules are not loaded: $tbl"
    # No masquerade is an outage, not a breach, but still a preflight failure.
    printf '%s\n' "$tbl" | grep -qE 'ip saddr 10\.128\.0\.0/16 .*masquerade' \
      || fail "sandboxd_netns has no masquerade for the slot supernet — netns sandboxes have no egress"
    # VM -> host: the drop, and the established,related accept that must sit above it.
    drop_at=$(line_of "$tbl" 'iifname "vh\*" ip saddr 10\.128\.0\.0/16 drop')
    accept_at=$(line_of "$tbl" 'ip saddr 10\.128\.0\.0/16 ct state established,related accept')
    [ -n "$drop_at" ] || fail "sandboxd_netns input drop (iifname vh*, saddr 10.128/16) missing — a guest can open connections to the host"
    [ -n "$accept_at" ] || fail "sandboxd_netns established,related accept missing — the node cannot reach envd in any sandbox"
    [ "$accept_at" -lt "$drop_at" ] \
      || fail "the sandboxd_netns established,related accept is BELOW the vh* drop (line $accept_at vs $drop_at) — it never matches"
    # Host-side second layer of the inter-slot and metadata blocks; the in-ns
    # rules are the primary enforcement.
    printf '%s\n' "$tbl" | grep -qE 'ip saddr 10\.128\.0\.0/16 ip daddr 10\.128\.0\.0/16 drop' \
      || fail "sandboxd_netns slot-to-slot forward drop missing"
    printf '%s\n' "$tbl" | grep -qE 'ip saddr 10\.128\.0\.0/16 ip daddr 169\.254\.0\.0/16 drop' \
      || fail "sandboxd_netns metadata forward drop missing"
    # The base filter FORWARD chain is a SEPARATE netfilter hook from the table
    # above, and a drop there wins however correct that table is. Installing
    # docker sets its policy to DROP, and provision-node.sh installs docker on
    # every node because docker is what builds the template rootfs images —
    # so this is the default configuration, not an exotic one. The symptom is a
    # sandbox with no outbound network at all, in either direction.
    fwd="$(iptables -S FORWARD 2>/dev/null || true)"
    if printf '%s\n' "$fwd" | grep -qE -- '^-P FORWARD DROP'; then
      du="$(iptables -S DOCKER-USER 2>/dev/null || true)"
      base="$fwd
$du"
      printf '%s\n' "$base" | grep -qE -- '-s 10\.128\.0\.0/16 -j ACCEPT' \
        || fail "FORWARD policy is DROP and nothing in FORWARD or DOCKER-USER accepts -s 10.128.0.0/16 — netns sandboxes have NO outbound network at all (not a DNS problem: no connectivity at any layer). EnsureNetNSHostRules installs this pair; a docker install is what set the policy"
      printf '%s\n' "$base" | grep -qE -- '-d 10\.128\.0\.0/16 .*-j ACCEPT' \
        || fail "FORWARD policy is DROP and nothing in FORWARD or DOCKER-USER accepts replies to -d 10.128.0.0/16 — sandbox packets leave and no reply is forwarded back"
    fi ;;
  *) fail "usage: verify-egress.sh [--firecracker|--docker|--netns]" ;;
esac
echo "egress rules present and correctly ordered ($mode)."

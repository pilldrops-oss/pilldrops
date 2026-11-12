#!/usr/bin/env bash
# security-suite.sh: the sandbox reachability and containment matrix as an
# executable check. Three rules: assert the property by attempting the
# connection, not by grepping rules; a probe that could not run is SKIP, never
# PASS; every negative probe has a positive control so a sandbox with no
# network cannot pass vacuously.
#
# Usage: security-suite.sh --runtime <firecracker|docker|gvisor> [options]; see --help.
# Exit codes: 0 all good, 1 at least one FAIL, 2 usage/setup error.
set -uo pipefail

HOST="${SECSUITE_HOST:-http://localhost:18080}"
KEY="${PILLDROPS_API_KEY:-}"
# Node /metrics is node-wide, so only an operator key reads it.
OPERATOR_KEY="${PILLDROPS_OPERATOR_KEY:-}"
TEMPLATE="${SECSUITE_TEMPLATE:-probe}"
# IMAGE creates from a Docker image instead of a template: the docker runtime
# never reads templates/<id>/Dockerfile, so `--template probe` has no tools there.
IMAGE=""
RUNTIME=""
STRICT=0
# Same default as envd's PD_TERMINAL_PORT; overridable so the terminal probes
# can be pointed at a deliberately unauthenticated terminal.
TERM_PORT="${SECSUITE_TERMINAL_PORT:-7681}"

usage() {
  cat <<'EOF'
usage: security-suite.sh --runtime <firecracker|docker|gvisor> [options]

  --runtime R    required; the runtime under test. Some probes are
                 runtime-specific and skip cleanly on the others.
  --host URL     API base (default $SECSUITE_HOST or http://localhost:18080)
  --key K        API key (default $PILLDROPS_API_KEY)
  --operator-key K
                 operator key for node /metrics (default
                 $PILLDROPS_OPERATOR_KEY, then --key). If /metrics refuses
                 it, the disk probe SKIPs when a write passes the cap, since
                 only /metrics tells a limit the node detected from a silent one.
  --template T   probe template (default "probe"; see templates/probe/)
  --image TAG    create from a Docker image instead of a template. Use this on
                 the docker runtime, where templates/<id>/Dockerfile is not
                 consulted:  docker build -t pilldrops-probe templates/probe
  --strict       also fail on a SKIP whose reason is not "not applicable to
                 this runtime". Use this in CI: it turns "the suite stopped
                 testing things" into a failure instead of a quiet pass.
  --self-test    run only the verdict named by SELFTEST_TERMINAL_URL or
                 SELFTEST_DISK_WROTE, to show a probe failing. Without this
                 flag those variables are ignored, so a stray one cannot
                 shorten a real run.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --runtime) RUNTIME="${2:-}"; shift 2 ;;
    --host)    HOST="${2:-}"; shift 2 ;;
    --key)     KEY="${2:-}"; shift 2 ;;
    --operator-key) OPERATOR_KEY="${2:-}"; shift 2 ;;
    --template) TEMPLATE="${2:-}"; shift 2 ;;
    --image)   IMAGE="${2:-}"; shift 2 ;;
    --strict)  STRICT=1; shift ;;
    --self-test) SELF_TEST=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

# With --self-test, SELFTEST_TERMINAL_URL runs only the terminal verdict (no
# node, key or runtime needed) and SELFTEST_DISK_WROTE only the over-cap disk
# verdict against --host. Without it they are dropped: a real run is never cut short.
if [ "${SELF_TEST:-}" = 1 ]; then
  [ -n "${SELFTEST_TERMINAL_URL:-}${SELFTEST_DISK_WROTE:-}" ] \
    || { echo "--self-test needs SELFTEST_TERMINAL_URL or SELFTEST_DISK_WROTE" >&2; exit 2; }
else
  unset SELFTEST_TERMINAL_URL SELFTEST_DISK_WROTE
fi
if [ -z "${SELFTEST_TERMINAL_URL:-}" ] && [ -z "${SELFTEST_DISK_WROTE:-}" ]; then
  [ -n "$RUNTIME" ] || { echo "--runtime is required" >&2; usage; exit 2; }
  [ -n "$KEY" ] || { echo "no API key: set PILLDROPS_API_KEY or pass --key" >&2; exit 2; }
  command -v jq >/dev/null || { echo "jq is required on the machine running this suite" >&2; exit 2; }
fi

PASS=0; FAIL=0; SKIP=0; SKIP_DISALLOWED=0
SANDBOX_LOG="$(mktemp -t security-suite-sandboxes.XXXXXX)"
# Each probe emits one machine-readable line:  RESULT|<outcome>|<id>|<detail>
emit() { printf 'RESULT|%s|%s|%s\n' "$1" "$2" "$3"; }

pass() { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m  %-42s %s\n' "$1" "${2:-}"; emit PASS "$1" "${2:-}"; }
fail() { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m  %-42s %s\n' "$1" "${2:-}"; emit FAIL "$1" "${2:-}"; }

# skip <id> <reason> [na]; "na" (not applicable to this runtime) is the only
# skip --strict tolerates.
skip() {
  SKIP=$((SKIP+1))
  local kind="${3:-blocked}"
  if [ "$kind" != "na" ]; then SKIP_DISALLOWED=$((SKIP_DISALLOWED+1)); fi
  printf '  \033[33mSKIP\033[0m  %-42s %s\n' "$1" "$2"
  emit "SKIP:$kind" "$1" "$2"
}

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

api() { # api METHOD PATH [BODY]
  if [ $# -ge 3 ]; then
    curl -sS -X "$1" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
      --data-binary "$3" "$HOST$2"
  else
    curl -sS -X "$1" -H "Authorization: Bearer $KEY" "$HOST$2"
  fi
}

sb_create() { # sb_create -> id (empty on failure)
  local body id
  if [ -n "$IMAGE" ]; then
    body=$(jq -nc --arg i "$IMAGE" '{image:$i}')
  else
    body=$(jq -nc --arg t "$TEMPLATE" '{template:$t}')
  fi
  id=$(api POST /v1/sandboxes "$body" | jq -r '.id // empty')
  [ -n "$id" ] && printf '%s\n' "$id" >> "$SANDBOX_LOG"
  printf '%s' "$id"
}

sb_create_sized() { # sb_create_sized STORAGE_MB -> id
  local body id
  if [ -n "$IMAGE" ]; then
    body=$(jq -nc --arg i "$IMAGE" --argjson s "$1" '{image:$i,storage_mb:$s}')
  else
    body=$(jq -nc --arg t "$TEMPLATE" --argjson s "$1" '{template:$t,storage_mb:$s}')
  fi
  id=$(api POST /v1/sandboxes "$body" | jq -r '.id // empty')
  [ -n "$id" ] && printf '%s\n' "$id" >> "$SANDBOX_LOG"
  printf '%s' "$id"
}

sb_exec() { # sb_exec ID CMD -> stdout+stderr of the guest command
  api POST "/v1/sandboxes/$1/exec" "$(jq -nc --arg c "$2" '{cmd:$c,timeout:60}')" \
    | jq -r '((.stdout // "") + (.stderr // ""))'
}

# sb_exec_t ID CMD MAXSECS is sb_exec with a client-side ceiling, for probes
# whose subject may be dead: an exec against a darkened guest otherwise hangs
# on TCP retransmission with no deadline anywhere in the path.
sb_exec_t() {
  curl -s -m "$3" -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    --data-binary "$(jq -nc --arg c "$2" '{cmd:$c,timeout:60}')" "$HOST/v1/sandboxes/$1/exec" \
    | jq -r '((.stdout // "") + (.stderr // ""))'
}

# The registry is a file, not an array: sb_create runs inside `$(...)`, and an
# array appended in that subshell is lost.
cleanup() {
  [ -n "${SANDBOX_LOG:-}" ] && [ -f "$SANDBOX_LOG" ] || return 0
  local id
  while read -r id; do
    [ -n "$id" ] && api DELETE "/v1/sandboxes/$id" >/dev/null 2>&1
  done < "$SANDBOX_LOG"
  rm -f "$SANDBOX_LOG"
}
trap cleanup EXIT

# rc_of runs a command in the sandbox that prints "rc=<n>" and returns just n.
rc_of() { sb_exec "$1" "$2"'; echo rc=$?' | grep -o 'rc=[0-9]*' | tail -1 | cut -d= -f2; }

# expect_blocked <id> <label> <sandbox-id> <cmd> <expected-codes...>
# A dropped route times out (curl 28); an on-link peer that cannot be ARPed fails
# immediately (curl 7). Accepting any non-zero would let a sandbox with no network pass.
expect_blocked() {
  local id="$1" label="$2" sbx="$3" cmd="$4"; shift 4
  local rc; rc=$(rc_of "$sbx" "$cmd")
  [ -n "$rc" ] || { fail "$id" "$label: probe produced no exit code"; return; }
  for want in "$@"; do
    if [ "$rc" = "$want" ]; then pass "$id" "$label (rc=$rc)"; return; fi
  done
  if [ "$rc" = "0" ]; then
    fail "$id" "$label: REACHABLE (rc=0) — this should be blocked"
  else
    fail "$id" "$label: blocked but with rc=$rc, expected one of [$*] — different failure mode than the control implies"
  fi
}

# classify_ws_code <id> <http-code> <label>
# 401 is the only acceptable answer; 000 (no response) must never read as auth enforced.
# One function so the self-test runs this logic rather than a copy.
classify_ws_code() {
  case "$2" in
    401)    pass "$1" "$3 returned 401" ;;
    000|"") fail "$1" "$3: no HTTP response despite the port being bound" ;;
    101)    fail "$1" "$3 UPGRADED (101) — a root PTY for anyone who can reach the port" ;;
    *)      fail "$1" "$3 returned $2, want 401 — a token is not being required before the upgrade" ;;
  esac
}

DISK_CAP_MB=1024
DISK_TRY_MB=3000

# disk_over_cap <bytes-written>: a write past the cap fails only when the node
# does not know, since a driver that accepts --storage-opt and ignores it must
# be detected and reported. One function so the self-test runs this logic.
disk_over_cap() {
  local wrote_mb=$(( $1 / 1000000 )) metrics code unenf
  metrics=$(curl -sS -w '\n%{http_code}' -H "Authorization: Bearer ${OPERATOR_KEY:-$KEY}" "$HOST/metrics")
  code=${metrics##*$'\n'}
  if [ "$code" != "200" ]; then
    skip "exhaust.disk" "wrote $wrote_mb MB into a declared ${DISK_CAP_MB}MB cap, but node /metrics answered $code, so whether the node detected it is unknown; /metrics needs an operator key (--operator-key or PILLDROPS_OPERATOR_KEY)"
    return
  fi
  unenf=$(printf '%s\n' "${metrics%$'\n'*}" | awk '/^sandboxd_storage_cap_unenforced_total /{print $2}' | tail -1)
  if [ -n "$unenf" ] && [ "${unenf%.*}" -gt 0 ] 2>/dev/null; then
    skip "exhaust.disk" "this host's storage driver cannot enforce the cap (wrote $wrote_mb MB of a declared ${DISK_CAP_MB}MB) — the node detected it at boot and reports sandboxd_storage_cap_unenforced_total=$unenf; a production node refuses to run in this state" na
  else
    fail "exhaust.disk" "wrote $wrote_mb MB into a declared ${DISK_CAP_MB}MB cap and the node reports NO unenforced-cap counter — the cap is silently ineffective"
  fi
}

if [ -n "${SELFTEST_DISK_WROTE:-}" ]; then
  echo "security-suite: disk over-cap SELF-TEST against ${HOST}"
  disk_over_cap "$SELFTEST_DISK_WROTE"
  printf '\nsummary  pass=%d fail=%d skip=%d\n' "$PASS" "$FAIL" "$SKIP"
  [ "$FAIL" -gt 0 ] && exit 1
  exit 0
fi

# Self-test: run the terminal verdict against any URL so the probe can be shown failing.
if [ -n "${SELFTEST_TERMINAL_URL:-}" ]; then
  echo "security-suite: terminal probe SELF-TEST against ${SELFTEST_TERMINAL_URL}"
  CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 6 "${SELFTEST_TERMINAL_URL}/ws" 2>/dev/null)
  classify_ws_code "term.ws_requires_token" "$CODE" "unauthenticated /ws"
  printf '\nsummary  pass=%d fail=%d\n' "$PASS" "$FAIL"
  [ "$FAIL" -gt 0 ] && exit 1
  exit 0
fi

echo "security-suite: runtime=$RUNTIME host=$HOST source=${IMAGE:-template:$TEMPLATE} strict=$STRICT"

section "0. Preconditions"

if ! api GET /health >/dev/null 2>&1; then
  echo "cannot reach $HOST/health — is the node running?" >&2; exit 2
fi
pass "api.reachable" "$HOST"

if [ -n "$IMAGE" ]; then
  pass "probe.source" "image $IMAGE"
else
  BOOTABLE=$(api GET /v1/templates | jq -r 'if type=="array" then .[].id else (.templates//[])[].id end' 2>/dev/null | tr '\n' ' ')
  case " $BOOTABLE " in
    *" $TEMPLATE "*) pass "probe.source" "template $TEMPLATE" ;;
    *) echo "template '$TEMPLATE' is not listed by this node. Build it first:" >&2
       echo "  sudo bash scripts/build-template.sh probe" >&2
       exit 2 ;;
  esac
fi

PROBE=$(sb_create)
[ -n "$PROBE" ] || { echo "could not create a probe sandbox from template '$TEMPLATE'" >&2; exit 2; }
pass "probe.created" "$PROBE"

# Assert the tools exist before believing any result they produce.
MISSING=$(sb_exec "$PROBE" 'for b in curl ping nc ip; do command -v $b >/dev/null 2>&1 || printf "%s " $b; done')
MISSING=$(printf '%s' "$MISSING" | tr -d '\r\n ')
if [ -n "$MISSING" ]; then
  echo "probe image is missing tools: $MISSING" >&2
  echo "every probe below would report 'blocked' by producing no traffic at all." >&2
  exit 2
fi
pass "probe.tools" "curl ping nc ip all present"

# In netns mode there is no shared bridge or FDB, so section 3 skips and section 3n
# probes the netns mechanisms instead. SECSUITE_NETNS=1/0 overrides auto-detection
# so each mode's rows can be forced against the other mode's node.
NETNS_MODE=0
if [ "$RUNTIME" = "firecracker" ]; then
  case "${SECSUITE_NETNS:-auto}" in
    1) NETNS_MODE=1 ;;
    0) ;;
    *) [ "$(ip netns list 2>/dev/null | grep -c '^fcns-')" -gt 0 ] && NETNS_MODE=1 ;;
  esac
fi

section "1. Positive controls (a sandbox with no network must not pass §2)"

RC=$(rc_of "$PROBE" 'curl -s -o /dev/null -m 15 https://example.com')
if [ "$RC" = "0" ]; then pass "egress.internet" "https://example.com reachable"
else fail "egress.internet" "outbound HTTPS failed (rc=$RC) — every 'blocked' result below is meaningless"; fi

RC=$(rc_of "$PROBE" 'getent hosts example.com >/dev/null')
if [ "$RC" = "0" ]; then pass "egress.dns" "resolver works"
else fail "egress.dns" "DNS resolution failed (rc=$RC)"; fi

# Routed destinations are dropped, so curl times out: rc=28.
section "2. Reachability — must be blocked"

expect_blocked "block.metadata.aws_gcp"  "169.254.169.254 (AWS/GCP metadata)" "$PROBE" 'curl -s -o /dev/null -m 6 http://169.254.169.254/' 28 7
expect_blocked "block.metadata.scaleway" "169.254.42.42 (Scaleway metadata)"  "$PROBE" 'curl -s -o /dev/null -m 6 http://169.254.42.42/' 28 7
expect_blocked "block.metadata.azure"    "169.254.169.254/metadata (Azure)"   "$PROBE" 'curl -s -o /dev/null -m 6 -H "Metadata:true" http://169.254.169.254/metadata/instance' 28 7
expect_blocked "block.rfc1918.10"        "10.0.0.1 (RFC1918)"                 "$PROBE" 'curl -s -o /dev/null -m 6 http://10.0.0.1/' 28 7
expect_blocked "block.rfc1918.172"       "172.17.0.1 (docker0 / RFC1918)"     "$PROBE" 'curl -s -o /dev/null -m 6 http://172.17.0.1/' 28 7
expect_blocked "block.cgnat"             "100.64.0.1 (CGNAT)"                 "$PROBE" 'curl -s -o /dev/null -m 6 http://100.64.0.1/' 28 7
# IPv6 is asserted absent, not filtered: every rule the platform installs is
# IPv4-family, so the fix is disabling the stack. /proc/net/if_inet6 lists
# an address whenever the stack is up; with disable_ipv6=1 newer kernels keep
# the file but leave it empty, so it is tested for content, not existence.
# grep, not test -s: a /proc file always reports size 0. rc 1 is an empty
# file, rc 2 no file at all (no IPv6 in the kernel); both mean no stack.
expect_blocked "block.ipv6.stack"        "guest has NO IPv6 stack (/proc/net/if_inet6)" "$PROBE" 'grep -q . /proc/net/if_inet6' 1 2
expect_blocked "block.ipv6.linklocal"    "guest has no IPv6 address on any interface"   "$PROBE" 'ip -6 addr show scope link 2>/dev/null | grep -q inet6' 1
# tcp/22 with a node-side control: the target must demonstrably accept tcp/22
# from this network or "blocked" from the guest means nothing.
SSH_TARGET="${SECSUITE_SSH_TARGET:-github.com}"
if timeout 8 bash -c "exec 3<>/dev/tcp/$SSH_TARGET/22" 2>/dev/null; then
  RC=$(rc_of "$PROBE" "nc -z -w 6 $SSH_TARGET 22")
  if [ "$RC" = "0" ]; then
    fail "block.port.ssh" "guest CONNECTED to $SSH_TARGET:22 — the egress allow-list is not holding"
  elif [ -z "$RC" ]; then
    fail "block.port.ssh" "probe produced no exit code"
  else
    pass "block.port.ssh" "tcp/22 to $SSH_TARGET blocked from the guest (rc=$RC) while the node itself connects (control)"
  fi
else
  skip "block.port.ssh" "the node itself cannot reach $SSH_TARGET:22, so a guest block would be vacuous (set SECSUITE_SSH_TARGET)"
fi

# The node's own API, at the gateway address a sandbox can derive for itself.
GW=$(sb_exec "$PROBE" 'ip route | awk "/^default/ {print \$3; exit}"' | tr -d '\r\n ')
if [ -n "$GW" ]; then
  expect_blocked "block.node.api" "node API at gateway $GW:18080" "$PROBE" \
    "curl -s -o /dev/null -m 6 http://$GW:18080/health" 28 7
else
  skip "block.node.api" "could not determine the guest's default gateway"
fi

# Without a real listener, an isolated network and an empty port look alike.
section "3. Sandbox to sandbox"

PEER=$(sb_create)
if [ "$NETNS_MODE" = "1" ]; then
  # In netns mode every guest holds the same fixed address, so dialling "the
  # peer's IP" reaches the prober's own services. PEER stays created for 3n.
  for id in peer.listener block.peer.tcp block.peer.envd block.peer.terminal block.peer.icmp; do
    skip "$id" "netns mode: peers share one in-ns address; cross-sandbox reachability is probed at the slot address (netns.peer_slot_blocked)" na
  done
elif [ -z "$PEER" ]; then
  skip "block.peer.tcp" "could not create a second sandbox"
else
  PEER_IP=$(sb_exec "$PEER" 'ip -4 -o addr show scope global | awk "{print \$4}" | cut -d/ -f1 | head -1' | tr -d '\r\n ')
  if [ -z "$PEER_IP" ]; then
    skip "block.peer.tcp" "could not read the peer's address"
  else
    # Confirm the listener is bound before trusting a refusal.
    sb_exec "$PEER" 'nohup nc -l -p 9999 -k >/dev/null 2>&1 & sleep 1; echo started' >/dev/null
    BOUND=$(sb_exec "$PEER" 'ss -ltn 2>/dev/null | grep -c ":9999" || true' | tr -d '\r\n ')
    if [ "${BOUND:-0}" = "0" ]; then
      skip "block.peer.tcp" "listener never bound on the peer, so a refusal would prove nothing"
    else
      pass "peer.listener" "peer $PEER_IP has :9999 bound (control for the next probe)"
      # On-link peers fail ARP and curl reports 7 rather than timing out.
      expect_blocked "block.peer.tcp" "peer $PEER_IP:9999 (listener IS bound)" "$PROBE" \
        "curl -s -o /dev/null -m 6 http://$PEER_IP:9999/" 28 7
    fi
    expect_blocked "block.peer.envd" "peer $PEER_IP:6001 (envd)" "$PROBE" \
      "curl -s -o /dev/null -m 6 http://$PEER_IP:6001/" 28 7

    expect_blocked "block.peer.terminal" "peer $PEER_IP:${TERM_PORT} (web terminal)" "$PROBE" \
      "curl -s -o /dev/null -m 6 http://$PEER_IP:${TERM_PORT}/" 28 7

    # ping needs CAP_NET_RAW; rc=126/127 means no packet was sent, not that ICMP is blocked.
    RC=$(rc_of "$PROBE" "ping -c 2 -W 3 $PEER_IP >/dev/null 2>&1")
    case "$RC" in
      0)       fail "block.peer.icmp" "ICMP to peer SUCCEEDED — peers are on-link and reachable" ;;
      126|127) skip "block.peer.icmp" "ping could not execute (rc=$RC) — CAP_NET_RAW is dropped on this runtime, so no ICMP was sent" na ;;
      "")      skip "block.peer.icmp" "probe produced no exit code" ;;
      *)       pass "block.peer.icmp" "ICMP to $PEER_IP sent and unanswered (rc=$RC)" ;;
    esac
  fi
fi

# On Firecracker guests are L2 neighbours on fcbr0, so bridge port isolation,
# not a FORWARD rule, is the enforcement.
if [ "$RUNTIME" != "firecracker" ]; then
  skip "peer.bridge_isolation" "firecracker-only mechanism" na
elif [ "$NETNS_MODE" = "1" ]; then
  skip "peer.bridge_isolation" "netns mode: no shared bridge exists to isolate (see §3n)" na
elif command -v bridge >/dev/null 2>&1 && [ "$(id -u)" = "0" ]; then
  ISO=$(bridge -d link show 2>/dev/null | grep -c "isolated on" || true)
  if [ "${ISO:-0}" -gt 0 ]; then pass "peer.bridge_isolation" "$ISO bridge port(s) with 'isolated on'"
  else fail "peer.bridge_isolation" "no bridge port has 'isolated on' — VM-to-VM rests on an L3 rule bridged frames never traverse"; fi
else
  skip "peer.bridge_isolation" "needs root on the node (host-side check)"
fi

# FDB source-MAC poisoning: on a learning bridge port a guest sourcing frames from a
# neighbour's MAC moves that MAC to its own tap and the neighbour goes dark, so guest
# MACs must be permanent FDB entries with learning off (peer.fdb_pinned is the control).
if [ "$RUNTIME" != "firecracker" ]; then
  skip "peer.fdb_pinned" "firecracker-only mechanism (no shared fcbr0)" na
  skip "peer.fdb_poison" "firecracker-only mechanism (no shared fcbr0)" na
elif [ "$NETNS_MODE" = "1" ]; then
  skip "peer.fdb_pinned" "netns mode: no shared FDB exists to pin an entry in (netns.flood_immune covers the property)" na
  skip "peer.fdb_poison" "netns mode: no shared FDB exists to poison (netns.flood_immune covers the property)" na
elif ! command -v bridge >/dev/null 2>&1 || [ "$(id -u)" != "0" ]; then
  skip "peer.fdb_pinned" "needs root on the node (host-side FDB check)"
  skip "peer.fdb_poison" "needs root on the node (host-side FDB check)"
else
  fdb_tap() { bridge fdb show br fcbr0 2>/dev/null | grep -i "$1" | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}'; }
  ATT=$(sb_create); VIC=$(sb_create)
  if [ -z "$ATT" ] || [ -z "$VIC" ]; then
    skip "peer.fdb_pinned" "could not create attacker+victim sandboxes"
    skip "peer.fdb_poison" "could not create attacker+victim sandboxes"
  else
    VMAC=$(sb_exec "$VIC" 'cat /sys/class/net/eth0/address' | tr -d '\r\n ')
    AMAC=$(sb_exec "$ATT" 'cat /sys/class/net/eth0/address' | tr -d '\r\n ')
    GWF=$(sb_exec "$ATT" 'ip route | awk "/^default/ {print \$3; exit}"' | tr -d '\r\n ')
    VTAP=$(fdb_tap "$VMAC"); ATAP=$(fdb_tap "$AMAC")
    if [ -n "$VMAC" ] && bridge fdb show br fcbr0 2>/dev/null | grep -i "$VMAC" | grep -q "permanent"; then
      pass "peer.fdb_pinned" "victim MAC $VMAC pinned permanent on $VTAP"
    else
      fail "peer.fdb_pinned" "victim MAC $VMAC is not pinned permanent — a learned entry is movable"
    fi
    # Victim reachable before, so a later "unreachable" is meaningful.
    VUP=$(sb_exec "$VIC" 'echo up' | tr -d '\r\n ')
    ARX0=$(cat /sys/class/net/"$ATAP"/statistics/rx_packets 2>/dev/null)
    # Detached so the attacker survives its own connectivity loss.
    sb_exec "$ATT" "setsid nohup sh -c 'ip link set eth0 down; ip link set eth0 address $VMAC; ip link set eth0 up; ping -f -w 12 -I eth0 $GWF' >/dev/null 2>&1 </dev/null & echo go" >/dev/null 2>&1
    sleep 3
    ARX1=$(cat /sys/class/net/"$ATAP"/statistics/rx_packets 2>/dev/null)
    MOVED=no; UNREACH=no
    for s in 1 2 3 4 5 6; do
      [ "$(fdb_tap "$VMAC")" = "$ATAP" ] && MOVED=yes
      [ "$(sb_exec_t "$VIC" 'echo up' 8 | tr -d '\r\n ')" = "up" ] || UNREACH=yes
      sleep 1
    done
    if [ -z "$ARX0" ] || [ -z "$ARX1" ] || [ "$ARX1" -le "$ARX0" ]; then
      # No frames on the wire, so "unmoved" proves nothing.
      skip "peer.fdb_poison" "attacker emitted no frames (rx ${ARX0:-?}->${ARX1:-?}); probe could not run"
    elif [ "$VUP" != "up" ]; then
      skip "peer.fdb_poison" "victim was unreachable before the attack; reachability leg would be vacuous"
    elif [ "$MOVED" = "yes" ] || [ "$UNREACH" = "yes" ]; then
      fail "peer.fdb_poison" "victim MAC $VMAC was stolen (moved=$MOVED unreachable=$UNREACH) — host->victim traffic hijacked"
    else
      pass "peer.fdb_poison" "victim MAC $VMAC stayed on $VTAP and reachable under a spoofing flood"
    fi
    api DELETE "/v1/sandboxes/$ATT" >/dev/null 2>&1
    api DELETE "/v1/sandboxes/$VIC" >/dev/null 2>&1
  fi
fi

# Same properties, netns mechanisms: the in-ns ruleset carries egress and inter-slot
# drops, the cross-sandbox vector is the node-facing slot address, and there is no
# shared FDB to poison. Every id below is emitted on every branch.
section "3n. Per-sandbox netns containment"

NETNS_IDS="netns.mode netns.filter_present netns.peer_drivable netns.peer_slot_blocked netns.host_listener netns.vm_host_blocked netns.mac_shared netns.flood_immune"
skipnetns() { local id; for id in $NETNS_IDS; do skip "$id" "$1" "${2:-}"; done; }

# fcns_slot_ip NAME OFFSET prints the slot's 10.128/16 address (offset 1 = host
# end of the veth, 2 = the ns end). The slot number comes from the namespace name.
fcns_slot_ip() {
  local s
  s=$(printf '%s' "$1" | sed -n 's/^fcns-\([0-9][0-9]*\)-.*/\1/p')
  [ -n "$s" ] || return 1
  printf '10.128.%d.%d' $(( s*4/256 )) $(( s*4%256 + $2 ))
}

# Sum of rx_packets across all fcns namespaces; the emitted-frames control for netns.flood_immune.
fcns_rx_sum() {
  local n t=0 v
  for n in $(ip netns list 2>/dev/null | awk '/^fcns-/{print $1}'); do
    v=$(ip netns exec "$n" sh -c \
      'for f in /sys/class/net/*/statistics/rx_packets; do case "$f" in */lo/*) ;; *) cat "$f";; esac; done' \
      2>/dev/null | awk '{s+=$1} END{print s+0}')
    t=$(( t + ${v:-0} ))
  done
  printf '%d' "$t"
}

if [ "$RUNTIME" != "firecracker" ]; then
  skipnetns "netns is a firecracker-only path" na
elif [ "$NETNS_MODE" != "1" ]; then
  skipnetns "node is on the shared-bridge path (no fcns-* namespaces)" na
elif [ "$(id -u)" != "0" ] || ! command -v nft >/dev/null 2>&1; then
  skipnetns "needs root + nft on the node (host-side checks)"
else
  # Control: bridge guests get counter-allocated addresses, never the fixed 192.168.30.2.
  NS_COUNT=$(ip netns list | grep -c '^fcns-')
  P_IP=$(sb_exec "$PROBE" 'ip -4 -o addr show scope global | awk "{print \$4}" | cut -d/ -f1 | head -1' | tr -d '\r\n ')
  if [ "$NS_COUNT" -gt 0 ] && [ "$P_IP" = "192.168.30.2" ]; then
    pass "netns.mode" "$NS_COUNT fcns namespace(s); probe guest holds the fixed 192.168.30.2"
  else
    fail "netns.mode" "expected per-sandbox namespaces with the fixed guest identity (fcns=$NS_COUNT guest_ip=${P_IP:-none})"
  fi

  # Control for the egress rows: every namespace carries a default-deny in-ns forward chain.
  NOFILTER=""
  for n in $(ip netns list | awk '/^fcns-/{print $1}'); do
    ip netns exec "$n" nft list chain ip sandbox forward 2>/dev/null | grep -q 'policy drop' \
      || NOFILTER="$NOFILTER $n"
  done
  if [ "$NS_COUNT" = "0" ]; then
    # Zero namespaces means the loop checked nothing.
    fail "netns.filter_present" "no fcns namespaces exist to carry a filter — nothing was checked"
  elif [ -z "$NOFILTER" ]; then
    pass "netns.filter_present" "all $NS_COUNT namespace(s) carry a default-deny in-ns forward chain"
  else
    fail "netns.filter_present" "namespaces WITHOUT a default-deny in-ns filter (finding A: every host egress rule is already bypassed):$NOFILTER"
  fi

  # The cross-sandbox vector is the node-facing slot address, which the peer's
  # namespace DNATs onto its envd:6001.
  NPEER="$PEER"
  [ -n "$NPEER" ] || NPEER=$(sb_create)
  if [ -z "$NPEER" ]; then
    skip "netns.peer_drivable" "could not create a peer sandbox"
    skip "netns.peer_slot_blocked" "no peer whose slot address a guest could aim at"
  else
    # Control: sb_exec reaches the peer through its slot address, so the addresses below are live.
    if [ "$(sb_exec "$NPEER" 'echo up' | tr -d '\r\n ')" = "up" ]; then
      pass "netns.peer_drivable" "peer $NPEER drivable via its slot address (control for the next probe)"
      REACHED=""; ODD=""; TRIED=0
      for n in $(ip netns list | awk '/^fcns-/{print $1}'); do
        A=$(fcns_slot_ip "$n" 2) || continue
        TRIED=$((TRIED+1))
        RC=$(rc_of "$PROBE" "curl -s -o /dev/null -m 6 http://$A:6001/")
        if [ "$RC" = "0" ]; then REACHED="$REACHED $A"
        elif [ "$RC" != "28" ] && [ "$RC" != "7" ]; then ODD="$ODD $A(rc=${RC:-none})"; fi
      done
      if [ "$TRIED" = "0" ]; then
        skip "netns.peer_slot_blocked" "no slot addresses parsed out of the namespace list"
      elif [ -n "$REACHED" ]; then
        fail "netns.peer_slot_blocked" "guest REACHED envd via slot address(es):$REACHED — the in-ns inter-slot drop is not holding"
      elif [ -n "$ODD" ]; then
        fail "netns.peer_slot_blocked" "blocked, but not the way the drop implies:$ODD (want rc=28/7)"
      else
        pass "netns.peer_slot_blocked" "$TRIED slot address(es), incl. the guest's own, all unreachable from a guest"
      fi
    else
      skip "netns.peer_drivable" "peer exists but is not drivable, so 'blocked' below would prove nothing"
      skip "netns.peer_slot_blocked" "no live peer envd behind a slot address"
    fi
  fi

  # VM -> host, probed against a host veth address the API is demonstrably
  # listening on; an unbound port would make "blocked" vacuous.
  FIRST_NS=$(ip netns list | awk '/^fcns-/{print $1; exit}')
  HOST_VETH_IP=$(fcns_slot_ip "$FIRST_NS" 1)
  API_PORT=$(printf '%s' "$HOST" | sed -n 's/.*:\([0-9][0-9]*\).*/\1/p'); API_PORT="${API_PORT:-18080}"
  if [ -z "$HOST_VETH_IP" ]; then
    skip "netns.host_listener" "could not derive a host veth address"
    skip "netns.vm_host_blocked" "could not derive a host veth address"
  elif curl -s -o /dev/null -m 3 "http://$HOST_VETH_IP:$API_PORT/health"; then
    pass "netns.host_listener" "node API answers on $HOST_VETH_IP:$API_PORT from the host (control)"
    expect_blocked "netns.vm_host_blocked" "host veth $HOST_VETH_IP:$API_PORT (node API)" "$PROBE" \
      "curl -s -o /dev/null -m 6 http://$HOST_VETH_IP:$API_PORT/health" 28 7
  else
    skip "netns.host_listener" "node API not reachable on $HOST_VETH_IP:$API_PORT from the host itself"
    skip "netns.vm_host_blocked" "the port is not bound on that address, so 'blocked' would be vacuous"
  fi

  # Every netns guest sources the same fixed MAC, so there is no per-guest L2 identity to steal.
  A_MAC=$(sb_exec "$PROBE" 'cat /sys/class/net/eth0/address' | tr -d '\r\n ')
  B_MAC=$(sb_exec "$NPEER" 'cat /sys/class/net/eth0/address' | tr -d '\r\n ')
  if [ -z "$A_MAC" ] || [ -z "$B_MAC" ]; then
    skip "netns.mac_shared" "could not read both guests' MACs"
  elif [ "$A_MAC" = "$B_MAC" ]; then
    pass "netns.mac_shared" "both guests source $A_MAC — no per-guest L2 identity exists to steal"
  else
    fail "netns.mac_shared" "guests source distinct MACs ($A_MAC vs $B_MAC) — a per-guest L2 identity exists and this section is not probing what it thinks"
  fi

  # A spoofed-source flood must stay contained to its namespace; the rx-sum
  # control refuses a vacuous pass from an attacker that never transmitted.
  NATT=$(sb_create)
  if [ -z "$NATT" ]; then
    skip "netns.flood_immune" "could not create an attacker sandbox"
  else
    RX0=$(fcns_rx_sum)
    sb_exec "$NATT" "setsid nohup sh -c 'ip link set eth0 down; ip link set eth0 address aa:fc:de:ad:be:ef; ip link set eth0 up; ping -f -w 10 192.168.30.1' >/dev/null 2>&1 </dev/null & echo go" >/dev/null 2>&1
    sleep 2
    UNREACH=no
    for s in 1 2 3 4 5; do
      [ "$(sb_exec_t "$NPEER" 'echo up' 8 | tr -d '\r\n ')" = "up" ] || UNREACH=yes
      sleep 1
    done
    RX1=$(fcns_rx_sum)
    if [ $(( RX1 - RX0 )) -lt 500 ]; then
      skip "netns.flood_immune" "attacker emitted too few frames (rx $RX0->$RX1); probe could not run"
    elif [ "$UNREACH" = "yes" ]; then
      fail "netns.flood_immune" "victim went undrivable while a neighbour flooded spoofed frames — containment did not hold"
    else
      pass "netns.flood_immune" "victim stayed drivable under a spoofed-source flood (rx +$(( RX1 - RX0 )) frames, all contained)"
    fi
    api DELETE "/v1/sandboxes/$NATT" >/dev/null 2>&1
  fi
fi

# Tag networks are opt-in: a sandbox reaches a peer only when both named the
# same tag. Every id below is emitted on every branch, and the negatives above
# (block.peer.*, netns.peer_slot_blocked) stay on UNTAGGED sandboxes, so the
# default this section opts out of is still measured beside it.
section "3t. Tag networks and private DNS"

TAGNET_IDS="peer.tagged block.peer.tag_mismatch dns.pd block.dns.pd_untagged"
skiptagnet() { local id; for id in $TAGNET_IDS; do skip "$id" "$1" "${2:-}"; done; }

# sb_create_net TAGS SLUG -> id. TAGS is a comma-separated list.
sb_create_net() {
  local body id
  body=$(jq -nc --arg t "$TEMPLATE" --arg i "$IMAGE" --arg s "$2" --arg n "$1" \
    '{networks:($n|split(",")),slug:$s} + (if $i == "" then {template:$t} else {image:$i} end)')
  id=$(api POST /v1/sandboxes "$body" | jq -r '.id // empty')
  [ -n "$id" ] && printf '%s\n' "$id" >> "$SANDBOX_LOG"
  printf '%s' "$id"
}

# sb_private_ip ID -> the address peers reach it on, empty when it is on no network.
sb_private_ip() { api GET "/v1/sandboxes/$1" | jq -r '.private_ip // empty'; }

TAGNET_OK=$(api GET /v1/capabilities | jq -r '.tag_networks // false')
# A unique suffix: slugs are unique platform-wide and a previous run's leftovers
# would otherwise take the name.
TAGSFX="$$"
if [ "$RUNTIME" != "firecracker" ]; then
  skiptagnet "tag networks are a firecracker-only feature (docs/runtimes.md)" na
elif [ "$TAGNET_OK" != "true" ]; then
  skiptagnet "this node reports tag_networks=false, so there is nothing to opt into"
else
  TA=$(sb_create_net "sec-t-$TAGSFX" "tagalpha$TAGSFX")
  TB=$(sb_create_net "sec-t-$TAGSFX" "tagbravo$TAGSFX")
  TC=$(sb_create_net "sec-u-$TAGSFX" "tagcharlie$TAGSFX")
  TB_IP=$(sb_private_ip "$TB")
  if [ -z "$TA" ] || [ -z "$TB" ] || [ -z "$TC" ] || [ -z "$TB_IP" ]; then
    skiptagnet "could not create the three tagged sandboxes (a=$TA b=$TB c=$TC ip=$TB_IP)"
  else
    # Confirm the listener is bound before trusting either a success or a refusal.
    sb_exec "$TB" 'nohup nc -l -p 9999 -k >/dev/null 2>&1 & sleep 1; echo started' >/dev/null
    TB_BOUND=$(sb_exec "$TB" 'ss -ltn 2>/dev/null | grep -c ":9999" || true' | tr -d '\r\n ')
    if [ "${TB_BOUND:-0}" = "0" ]; then
      skiptagnet "the listener never bound on the tagged peer, so neither a success nor a refusal would prove anything"
    else
      RC=$(rc_of "$TA" "nc -z -w 4 $TB_IP 9999 >/dev/null 2>&1")
      if [ "$RC" = "0" ]; then
        pass "peer.tagged" "sandboxes sharing tag sec-t-$TAGSFX connect ($TB_IP:9999, listener bound)"
      else
        fail "peer.tagged" "two sandboxes sharing a tag could NOT connect to $TB_IP:9999 (rc=$RC) — the opt-in does not work"
      fi
      expect_blocked "block.peer.tag_mismatch" "peer $TB_IP:9999 from a sandbox tagged sec-u-$TAGSFX" \
        "$TC" "nc -z -w 4 $TB_IP 9999 >/dev/null 2>&1" 1

      # DNS. getent prints the address on success and nothing on NXDOMAIN.
      PD=$(sb_exec "$TA" "getent hosts tagbravo$TAGSFX.pd" | tr -d '\r' | head -1)
      case "$PD" in
        *"$TB_IP"*) pass "dns.pd" "tagbravo$TAGSFX.pd resolves to the peer's private_ip $TB_IP" ;;
        "")         fail "dns.pd" "tagbravo$TAGSFX.pd did not resolve inside a sandbox that shares its tag" ;;
        *)          fail "dns.pd" "tagbravo$TAGSFX.pd resolved to '$PD', not the peer's private_ip $TB_IP" ;;
      esac
      # The untagged prober: same name, same node, no shared tag, no answer.
      PDU=$(sb_exec "$PROBE" "getent hosts tagbravo$TAGSFX.pd" | tr -d '\r' | head -1)
      PDC=$(rc_of "$PROBE" 'getent hosts example.com >/dev/null')
      if [ -n "$PDU" ]; then
        fail "block.dns.pd_untagged" "an untagged sandbox resolved tagbravo$TAGSFX.pd to '$PDU'"
      elif [ "$PDC" != "0" ]; then
        skip "block.dns.pd_untagged" "the untagged prober cannot resolve anything at all, so NXDOMAIN proves nothing"
      else
        pass "block.dns.pd_untagged" "untagged sandbox: tagbravo$TAGSFX.pd is NXDOMAIN while example.com still resolves (control)"
      fi
    fi
  fi
fi

section "4. Guest credential surface"

CMDLINE=$(sb_exec "$PROBE" 'cat /proc/cmdline')
case "$RUNTIME" in
  firecracker)
    # A cold boot puts the hash on the kernel cmdline; a snapshot restore
    # delivers it over vsock and the cmdline carries nothing. No cmdline hash
    # is only a failure when envd is also not enforcing a credential.
    HASH=$(printf '%s' "$CMDLINE" | grep -o 'envd.token_hash=[a-f0-9]*' | cut -d= -f2)
    if [ -n "$HASH" ]; then
      pass "cred.hash_present" "/proc/cmdline carries envd.token_hash (cold-boot delivery)"
    else
      WSCODE=$(sb_exec "$PROBE" "curl -s -o /dev/null -w '%{http_code}' -m 6 http://127.0.0.1:${TERM_PORT}/ws" | tr -d '\r\n ')
      if [ "$WSCODE" = "401" ]; then
        pass "cred.hash_present" "no cmdline hash (snapshot restore) and envd still refuses unauthenticated calls — the credential arrived out-of-band (vsock)"
      else
        fail "cred.hash_present" "no envd.token_hash on the kernel cmdline and no enforcement observed (/ws answered ${WSCODE:-nothing}, want 401) — envd auth may be off"
      fi
    fi
    # The guest sees a verifier, never a bearer token: presenting the hash must be rejected.
    if [ -n "$HASH" ]; then
      IP=$(sb_exec "$PROBE" 'ip -4 -o addr show scope global | awk "{print \$4}" | cut -d/ -f1 | head -1' | tr -d '\r\n ')
      RC=$(rc_of "$PROBE" "curl -s -o /dev/null -m 6 -H 'x-access-token: $HASH' http://$IP:6001/")
      if [ "$RC" != "0" ]; then pass "cred.hash_not_bearer" "the guest-visible hash is not accepted as a token (rc=$RC)"
      else fail "cred.hash_not_bearer" "envd ACCEPTED the guest-readable hash as a credential"; fi
    else
      pass "cred.hash_not_bearer" "no guest-visible verifier exists at all on the vsock-delivery path — nothing to present as a bearer"
    fi
    ;;
  *)
    skip "cred.hash_present"    "kernel cmdline delivery is firecracker-only" na
    skip "cred.hash_not_bearer" "kernel cmdline delivery is firecracker-only" na
    ;;
esac

# The property is a token required on the /ws upgrade; the page at / is only
# HTML. Confirm the port is bound first: "refused" and "auth enforced" look alike.
section "4b. Web terminal on :${TERM_PORT} requires a token"

TERM_BOUND=$(sb_exec "$PROBE" "ss -ltn 2>/dev/null | grep -c ':${TERM_PORT}' || true" | tr -d '\r\n ')
if [ "${TERM_BOUND:-0}" = "0" ]; then
  skip "term.listening" "nothing is bound on :${TERM_PORT} in this sandbox, so an unauthenticated request would fail for the wrong reason"
  skip "term.ws_requires_token" "no terminal to probe"
  skip "term.ws_rejects_wrong_token" "no terminal to probe"
else
  pass "term.listening" ":${TERM_PORT} is bound (control for the probes below)"

  CODE=$(sb_exec "$PROBE" "curl -s -o /dev/null -w '%{http_code}' -m 6 http://127.0.0.1:${TERM_PORT}/ws" | tr -d '\r\n ')
  classify_ws_code "term.ws_requires_token" "$CODE" "unauthenticated /ws"

  # A wrong token must fail exactly like no token, or the check is presence-of-header.
  CODE=$(sb_exec "$PROBE" "curl -s -o /dev/null -w '%{http_code}' -m 6 -H 'x-access-token: not-the-right-token' http://127.0.0.1:${TERM_PORT}/ws" | tr -d '\r\n ')
  classify_ws_code "term.ws_rejects_wrong_token" "$CODE" "/ws with an invalid token"
fi

# Shared-kernel runtimes only: a Firecracker guest is root of its own kernel
# and legitimately holds every capability, so the bounding set says nothing there.
section "5. Capability bounding set (shared-kernel runtimes)"

if [ "$RUNTIME" = "firecracker" ]; then
  skip "caps.bounding" "the guest has its own kernel; host capabilities are not the boundary" na
else
  CAPBND=$(sb_exec "$PROBE" 'grep ^CapBnd /proc/self/status | awk "{print \$2}"' | tr -d '\r\n ')
  if [ -z "$CAPBND" ]; then
    skip "caps.bounding" "could not read CapBnd"
  else
    # The six capabilities internal/runtime/docker deliberately does not add back.
    BAD=""
    for entry in "13:NET_RAW" "27:MKNOD" "31:SETFCAP" "8:SETPCAP" "18:SYS_CHROOT" "29:AUDIT_WRITE"; do
      bit="${entry%%:*}"; name="${entry##*:}"
      if [ "$(( 0x$CAPBND >> bit & 1 ))" = "1" ]; then BAD="$BAD $name"; fi
    done
    if [ -z "$BAD" ]; then pass "caps.bounding" "CapBnd=$CAPBND — none of the escalation caps present"
    else fail "caps.bounding" "CapBnd=$CAPBND retains:$BAD"; fi
  fi
fi

# Each probe gets a fresh sandbox and the fork bomb runs last, so no result
# depends on the ones before it.
section "6. Resource exhaustion is contained"

# Disk: declare a small cap, try to exceed it. Direct IO on purpose: a buffered
# write is OOM-killed by the memory cgroup long before the disk cap, which looks
# like containment and is not.
DISK_SBX=$(sb_create_sized "$DISK_CAP_MB")
if [ -z "$DISK_SBX" ]; then
  skip "exhaust.disk" "could not create a sandbox with a declared ${DISK_CAP_MB}MB cap"
else
  # fallocate first: it allocates without dirtying page cache, so a hard quota
  # answers ENOSPC; a buffered dd there can end in a memory-cgroup kill of the
  # whole sandbox and report nothing. dd stays as the fallback for an image
  # without fallocate. The file's size is the measurement either way.
  # Half the cap first, as the positive control: a disk that refuses every
  # allocation would otherwise pass as contained.
  HALF=$(sb_exec "$DISK_SBX" "if command -v fallocate >/dev/null; then fallocate -l $(( DISK_CAP_MB / 2 ))M \$HOME/half && echo HALF_OK; else echo HALF_OK; fi; rm -f \$HOME/half")
  DISK=$(sb_exec "$DISK_SBX" "if command -v fallocate >/dev/null; then fallocate -l ${DISK_TRY_MB}M \$HOME/fill 2>/dev/null; else timeout 180 dd if=/dev/zero of=\$HOME/fill bs=1M count=$DISK_TRY_MB oflag=direct >/dev/null 2>&1; fi; stat -c %s \$HOME/fill; rm -f \$HOME/fill")
  WROTE=$(printf '%s' "$DISK" | grep -o '^[0-9]*' | head -1)
  if ! printf '%s' "$HALF" | grep -q HALF_OK; then
    skip "exhaust.disk" "could not allocate half the cap ($(( DISK_CAP_MB / 2 )) MB), so a refusal above it proves nothing"
  elif ! api GET /health >/dev/null 2>&1; then
    fail "exhaust.disk" "node stopped answering after a large guest write"
  elif [ -z "$WROTE" ]; then
    skip "exhaust.disk" "could not read the size written: $(printf '%s' "$DISK" | tr -d '\r' | tail -1)"
  elif [ "$WROTE" -gt $(( DISK_CAP_MB * 1024 * 1024 * 12 / 10 )) ]; then
    disk_over_cap "$WROTE"
  else
    pass "exhaust.disk" "$(( DISK_CAP_MB / 2 )) MB fit; a $DISK_TRY_MB MB write stopped at $(( WROTE / 1000000 )) MB against a declared ${DISK_CAP_MB}MB cap"
  fi
fi

# Fork bomb last, in its own sandbox: it may leave that sandbox unable to accept execs.
FORK_SBX=$(sb_create)
if [ -z "$FORK_SBX" ]; then
  skip "exhaust.forkbomb" "could not create a dedicated sandbox"
else
  sb_exec "$FORK_SBX" 'timeout 15 bash -c ":(){ :|:& };:" >/dev/null 2>&1' >/dev/null
  if api GET /health >/dev/null 2>&1; then
    NEW=$(sb_create)
    if [ -n "$NEW" ]; then
      pass "exhaust.forkbomb" "node still serving AND still able to create sandboxes after a fork bomb"
    else
      fail "exhaust.forkbomb" "node answers /health but can no longer create a sandbox after a fork bomb"
    fi
  else
    fail "exhaust.forkbomb" "node stopped answering /health after a fork bomb in one sandbox"
  fi
fi

# The golden snapshot is the memory image every restored sandbox comes up holding,
# so a writer is node-wide and cross-tenant. The property is about the owner: a uid
# in the sandbox range could be handed to a tenant later even if none can write it now.
section "7. Golden snapshot is owned by no sandbox uid"

# Must match uidForSandbox in internal/vm/firecracker.go.
SB_UID_LO=10000
SB_UID_HI=59999

# Both ids are reported in every branch, including the ones that cannot run.
skip7() { skip "snapshot.golden_unowned" "$1" "${2:-}"; skip "snapshot.golden_unwritable" "$1" "${2:-}"; }

if [ "$RUNTIME" != "firecracker" ]; then
  skip7 "golden snapshots are a Firecracker-only path" na
elif ! command -v setpriv >/dev/null 2>&1; then
  skip7 "setpriv(1) not installed — the writability probe cannot be run"
else
  if [ "$(id -u)" = "0" ]; then AS_ROOT=""
  elif sudo -n true 2>/dev/null; then AS_ROOT="sudo -n"
  else AS_ROOT="unavailable"; fi

  SNAPDIR="${SNAPSHOT_DIR:-/var/lib/sandbox/templates}"
  if [ "$AS_ROOT" = "unavailable" ]; then
    skip7 "needs root (or passwordless sudo) to stat and to drop uid"
  elif [ ! -d "$SNAPDIR" ]; then
    skip7 "no template dir at $SNAPDIR — set SNAPSHOT_DIR"
  else
    GOLDEN=$($AS_ROOT find "$SNAPDIR" -path '*/snapshots/*' -type f \( -name mem -o -name state \) 2>/dev/null | sort)
    if [ -z "$GOLDEN" ]; then
      skip7 "no golden files under $SNAPDIR — FC_SNAPSHOTS is off, or SNAPSHOT_DIR points elsewhere" na
    else
      # Static: the owner must be root and no group/other write bit set.
      BAD=""; SEEN=0
      while read -r f; do
        [ -n "$f" ] || continue
        SEEN=$((SEEN+1))
        read -r U M <<EOF
$($AS_ROOT stat -c '%u %a' "$f")
EOF
        if [ "$U" != "0" ]; then
          if [ "$U" -ge "$SB_UID_LO" ] && [ "$U" -le "$SB_UID_HI" ] 2>/dev/null; then
            BAD="$BAD $f(uid=$U:ALLOCATABLE,mode=$M)"
          else
            BAD="$BAD $f(uid=$U:non-root,mode=$M)"
          fi
        elif [ $(( 0$M & 0022 )) -ne 0 ]; then
          BAD="$BAD $f(mode=$M:group/other-writable)"
        fi
      done <<EOF
$GOLDEN
EOF
      if [ -n "$BAD" ]; then
        fail "snapshot.golden_unowned" \
          "$SEEN golden file(s) checked; a uid this node can hand to a sandbox owns the image every restore resumes into:$BAD"
      else
        pass "snapshot.golden_unowned" "$SEEN golden file(s), all uid=0 with no group/other write"
      fi

      # Behavioural control: the same O_RDWR open as an unprivileged uid, with a
      # scratch file that uid can write and a read of the golden that must succeed.
      GF=$(printf '%s\n' "$GOLDEN" | head -1)
      # Probe as the golden's own owner when that is not root, else the top of the sandbox range.
      PU=$($AS_ROOT stat -c '%u' "$GF"); [ "$PU" = "0" ] && PU=$SB_UID_HI
      CTL=$(mktemp); $AS_ROOT chown "$PU" "$CTL"; $AS_ROOT chmod 600 "$CTL"
      as_uid() { $AS_ROOT setpriv --reuid="$1" --regid="$1" --clear-groups sh -c "$2" 2>/dev/null; }

      if ! as_uid "$PU" "exec 3<>'$CTL'"; then
        skip "snapshot.golden_unwritable" "positive control failed: uid $PU cannot write a file it owns, so a 'not writable' verdict would mean nothing"
      elif ! as_uid "$PU" "exec 3<'$GF'"; then
        fail "snapshot.golden_unwritable" "uid $PU cannot READ $GF — a restoring VMM opens this file; the node cannot restore"
      elif as_uid "$PU" "exec 3<>'$GF'"; then
        fail "snapshot.golden_unwritable" "uid $PU opened $GF O_RDWR — that uid is inside the range sandboxes are assigned from"
      else
        pass "snapshot.golden_unwritable" "uid $PU: control writable, golden readable, golden NOT writable"
      fi
      $AS_ROOT rm -f "$CTL"
    fi
  fi
fi

printf '\n\033[1msummary\033[0m  pass=%d fail=%d skip=%d\n' "$PASS" "$FAIL" "$SKIP"

if [ "$FAIL" -gt 0 ]; then
  echo "FAILED: $FAIL probe(s) observed a control that did not hold." >&2
  exit 1
fi
if [ "$STRICT" = "1" ] && [ "$SKIP_DISALLOWED" -gt 0 ]; then
  echo "FAILED (--strict): $SKIP_DISALLOWED probe(s) could not run for a reason other than" >&2
  echo "'not applicable to this runtime'. A suite that stopped testing is not a passing suite." >&2
  exit 1
fi
echo "OK"

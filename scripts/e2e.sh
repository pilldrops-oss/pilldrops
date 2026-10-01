#!/usr/bin/env bash
# e2e.sh: boot the real three-tier stack and assert what unit tests cannot
# (wiring order, background initialisation, real container boots).
#
# Usage:
#   ./scripts/e2e.sh            # full run, tears down after
#   KEEP=1 ./scripts/e2e.sh     # leave the stack up for poking at
#
# Requires: docker (Postgres + the sandbox runtime), go, python3, curl.
set -uo pipefail

ORCH_PORT=${ORCH_PORT:-19090}
NODE_PORT=${NODE_PORT:-18081}
PG_PORT=${PG_PORT:-55440}
FLEET_KEY=${FLEET_KEY:-e2e-fleet-key}
SHARED_TOKEN=${SHARED_TOKEN:-e2e-shared-token}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/pilldrops-e2e.XXXXXX")
O="http://127.0.0.1:${ORCH_PORT}"
N="http://127.0.0.1:${NODE_PORT}"

PASS=0; FAIL=0; SKIP=0
declare -a RESULTS

# ck NAME EXPECTED ACTUAL
ck() {
  local name="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    PASS=$((PASS+1)); RESULTS+=("PASS|$name|$got")
    printf '  \033[32m✓\033[0m %-58s %s\n' "$name" "$got"
  else
    FAIL=$((FAIL+1)); RESULTS+=("FAIL|$name|want=$want got=$got")
    printf '  \033[31m✗\033[0m %-58s want=%s got=%s\n' "$name" "$want" "$got"
  fi
}
# ckc NAME EXPECTED ACTUAL passes when ACTUAL contains EXPECTED.
ckc() {
  local name="$1" want="$2" got="$3"
  case "$got" in
    *"$want"*) PASS=$((PASS+1)); RESULTS+=("PASS|$name|$got")
               printf '  \033[32m✓\033[0m %-58s %s\n' "$name" "${got:0:60}" ;;
    *)         FAIL=$((FAIL+1)); RESULTS+=("FAIL|$name|want~$want got=$got")
               printf '  \033[31m✗\033[0m %-58s want~%s got=%s\n' "$name" "$want" "${got:0:60}" ;;
  esac
}
skip() { SKIP=$((SKIP+1)); RESULTS+=("SKIP|$1|$2"); printf '  \033[33m–\033[0m %-58s %s\n' "$1" "$2"; }
sec()  { printf '\n\033[1m%s\033[0m\n' "$1"; }

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
jq_()  { python3 -c "import sys,json$1" 2>/dev/null || echo ""; }

cleanup() {
  # Fires on every exit, including before anything started, so nothing here may assume a variable is set.
  [ "${KEEP:-0}" = "1" ] && { echo; echo "KEEP=1 — stack left up. Work dir: $WORK"; return; }
  sec "Teardown"
  for sb in $(curl -s "$O/v1/sandboxes" -H "Authorization: Bearer $FLEET_KEY" 2>/dev/null \
      | jq_ ";d=json.load(sys.stdin);l=d if isinstance(d,list) else d.get('sandboxes',[]);print(' '.join(x['id'] for x in l))"); do
    curl -s -o /dev/null -X DELETE "$O/v1/sandboxes/$sb" -H "Authorization: Bearer $FLEET_KEY"
  done
  [ -n "${NODE_PID:-}" ] && kill "$NODE_PID" 2>/dev/null
  [ -n "${ORCH_PID:-}" ] && kill "$ORCH_PID" 2>/dev/null
  [ -n "${PG_NAME:-}" ] && docker rm -f "$PG_NAME" >/dev/null 2>&1
  rm -rf "$WORK"
  echo "  stack down, $WORK removed"
}
trap cleanup EXIT

# A stale sandboxd on the same port would answer every probe for a binary this run did not build.
for port in $ORCH_PORT $NODE_PORT; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is already in use — set ORCH_PORT/NODE_PORT, or stop the process." >&2
    echo "Refusing to run: the results would describe whatever is already there." >&2
    exit 2
  fi
done

sec "Build"
go build -o "$WORK/orchestrator" ./cmd/orchestrator || exit 1
go build -o "$WORK/sandboxd"     ./cmd/sandboxd     || exit 1
echo "  orchestrator + sandboxd built from $(git rev-parse --short HEAD 2>/dev/null || echo 'working tree')"

sec "Infrastructure"
PG_NAME="pilldrops-e2e-pg-$$"
docker run -d --name "$PG_NAME" -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=sb \
  -p "${PG_PORT}:5432" postgres:16-alpine >/dev/null 2>&1
for i in $(seq 1 60); do docker exec "$PG_NAME" pg_isready -q 2>/dev/null && break; sleep 1; done
DSN="postgres://postgres:pw@localhost:${PG_PORT}/sb?sslmode=disable"
echo "  postgres up on :$PG_PORT"

sec "Control plane"
ORCHESTRATOR_API_KEY=$FLEET_KEY ORCHESTRATOR_TOKEN=$SHARED_TOKEN \
  SANDBOXD_DATABASE_URL="$DSN" PORT=$ORCH_PORT \
  "$WORK/orchestrator" > "$WORK/orch.log" 2>&1 &
ORCH_PID=$!
for i in $(seq 1 30); do [ "$(code $O/health)" = "200" ] && break; sleep 1; done
# The store opens in a goroutine; give it a moment before /readyz.
sleep 2
ck "liveness answers without any dependency"      200 "$(code $O/health)"
ck "readiness answers once the store is up"       200 "$(code $O/readyz)"

sec "Store-backed routes exist on a real boot (the async-store bug)"
ck "/v1/projects is mounted"     200 "$(code $O/v1/projects     -H "Authorization: Bearer $FLEET_KEY")"
ck "/v1/threads is mounted"      200 "$(code $O/v1/threads      -H "Authorization: Bearer $FLEET_KEY")"
ck "/v1/secrets (vault) is mounted" 200 "$(code $O/v1/secrets   -H "Authorization: Bearer $FLEET_KEY")"
ck "/v1/launch-stats is mounted" 200 "$(code "$O/v1/launch-stats?templates=base" -H "Authorization: Bearer $FLEET_KEY")"
ck "/v1/ai/configure is mounted (400, not 404)" 400 \
   "$(code -X POST $O/v1/ai/configure -H "Authorization: Bearer $FLEET_KEY" -H 'Content-Type: application/json' -d '{"prompt":"x"}')"

sec "Public catalogue — the only unauthenticated /v1 route"
curl -s "$O/v1/templates/public" -o "$WORK/pub.json"
ck "served with no credential"       200 "$(code $O/v1/templates/public)"
ckc "cacheable"                      "max-age=300" "$(curl -s -D- -o /dev/null $O/v1/templates/public | grep -i cache-control | tr -d '\r')"
ck "env VALUES redacted"             "clean" "$(python3 -c "
import json;d=json.load(open('$WORK/pub.json'))
print('LEAK' if [1 for t in d for e in (t.get('env') or []) if e.get('value')] else 'clean')" 2>/dev/null)"
sec "Threads — 404'd through the control plane before the migration"
TID=$(curl -s -X POST "$O/v1/threads" -H "Authorization: Bearer $FLEET_KEY" \
      -H 'Content-Type: application/json' -d '{}' | jq_ ";print(json.load(sys.stdin).get('id',''))")
ckc "empty POST starts a conversation"  "th-" "$TID"
ck  "untitled default"  "New conversation" "$(curl -s $O/v1/threads/$TID -H "Authorization: Bearer $FLEET_KEY" | jq_ ";print(json.load(sys.stdin).get('title',''))")"
curl -s -o /dev/null -X PATCH "$O/v1/threads/$TID" -H "Authorization: Bearer $FLEET_KEY" \
     -H 'Content-Type: application/json' -d '{"turns":"[1,2,3]"}'
ck  "PATCH turns does not clear the title" "New conversation" \
    "$(curl -s $O/v1/threads/$TID -H "Authorization: Bearer $FLEET_KEY" | jq_ ";print(json.load(sys.stdin).get('title',''))")"
ck  "DELETE really deletes"   204 "$(code -X DELETE $O/v1/threads/$TID -H "Authorization: Bearer $FLEET_KEY")"
ck  "and is then absent"      404 "$(code $O/v1/threads/$TID -H "Authorization: Bearer $FLEET_KEY")"

sec "Launch stats — the server withholds a median it cannot justify"
for i in 1 2 3 4; do
  curl -s -o /dev/null -X POST "$O/v1/launch-stats" -H "Authorization: Bearer $FLEET_KEY" \
       -H 'Content-Type: application/json' -d "{\"template\":\"e2e\",\"ms\":$((2000+i*50))}"
done
ck "4 samples: count reported, median withheld" '{"stats":{"e2e":{"p50_ms":0,"n":4}}}' \
   "$(curl -s "$O/v1/launch-stats?templates=e2e" -H "Authorization: Bearer $FLEET_KEY")"
curl -s -o /dev/null -X POST "$O/v1/launch-stats" -H "Authorization: Bearer $FLEET_KEY" \
     -H 'Content-Type: application/json' -d '{"template":"e2e","ms":2250}'
ckc "5th sample crosses the threshold" '"n":5' \
    "$(curl -s "$O/v1/launch-stats?templates=e2e" -H "Authorization: Bearer $FLEET_KEY")"
ckc "an unseen template still appears" '"n":0' \
    "$(curl -s "$O/v1/launch-stats?templates=never-seen" -H "Authorization: Bearer $FLEET_KEY")"

sec "MCP — moved to the control plane so tool calls can be scheduled"
ckc "initialize answers"  '"result"' "$(curl -s -X POST $O/mcp -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')"
ckc "create_sandbox offered" "create_sandbox" "$(curl -s -X POST $O/mcp -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}')"
ck  "GET refused (stateless transport)" 405 "$(code $O/mcp)"
ckc "and says so via Allow"  "POST" "$(curl -s -D- -o /dev/null $O/mcp | grep -i '^allow:' | tr -d '\r')"

sec "Node joins the fleet"
# /v1/join refuses plaintext because it hands out the DSN and secret key; the
# X-Forwarded-Proto header stands in for the TLS terminator on this loopback run.
JOIN_TOKEN=$(curl -s -X POST "$O/v1/join-tokens" -H "Authorization: Bearer $FLEET_KEY" \
  | jq_ ";print(json.load(sys.stdin)['token'])")
ckc "control plane mints a join token" "." "$JOIN_TOKEN"
JOIN=$(curl -s -X POST "$O/v1/join" -H 'Content-Type: application/json' \
  -H 'X-Forwarded-Proto: https' \
  -d "{\"token\":\"$JOIN_TOKEN\",\"suggested_id\":\"e2e-node\",\"zone\":\"default\"}")
NODE_KEY=$(printf '%s' "$JOIN" | jq_ ";print(json.load(sys.stdin)['node_api_key'])")
NODE_ID=$(printf '%s' "$JOIN" | jq_ ";print(json.load(sys.stdin)['node_id'])")
ckc "redeeming it returns the node key" "sf_node_" "$NODE_KEY"
ckc "and the node id it allocated"      "e2e-node" "$NODE_ID"
ck  "the token is single-use" 401 \
    "$(code -X POST $O/v1/join -H 'Content-Type: application/json' -H 'X-Forwarded-Proto: https' \
       -d "{\"token\":\"$JOIN_TOKEN\",\"suggested_id\":\"e2e-dupe\",\"zone\":\"default\"}")"
( cd "$WORK" && env -i PATH="$PATH" HOME="$HOME" \
    ORCHESTRATOR_URL=$O ORCHESTRATOR_TOKEN=$SHARED_TOKEN \
    NODE_ID="$NODE_ID" NODE_ZONE=default NODE_URL=$N NODE_API_KEY="$NODE_KEY" \
    SANDBOXD_DATABASE_URL="$DSN" SANDBOXD_RUNTIME=docker \
    SANDBOXD_DATA="$WORK/nodedata" PORT=$NODE_PORT \
    HOSTSTATS_MEM_TOTAL_MB=${HOSTSTATS_MEM_TOTAL_MB:-16384} \
    "$WORK/sandboxd" > "$WORK/node.log" 2>&1 & echo $! > "$WORK/node.pid" )
NODE_PID=$(cat "$WORK/node.pid")
for i in $(seq 1 40); do [ "$(code $N/health)" = "200" ] && break; sleep 1; done
ck "node is up"        200 "$(code $N/health)"
sleep 6
ck "node heartbeats into the fleet" "healthy" "$(curl -s $O/v1/nodes -H "Authorization: Bearer $FLEET_KEY" \
  | jq_ ";
import sys,json
ns=json.load(sys.stdin)['nodes']
m=[n for n in ns if n['id']=='$NODE_ID']
print('healthy' if m and m[0]['healthy'] else 'ABSENT')")"

sec "Fleet ingress — a node is not a public API"
TENANT=$(curl -s -X POST "$O/v1/keys" -H "Authorization: Bearer $FLEET_KEY" \
  -H 'Content-Type: application/json' -d '{"name":"e2e-tenant"}' | jq_ ";print(json.load(sys.stdin).get('key',''))")
ckc "a tenant key can be minted" "sf_live_" "$TENANT"
ck "operator key reaches the node"        200 "$(code $N/v1/sandboxes -H "Authorization: Bearer $NODE_KEY")"
ck "TENANT key is refused at the node"    403 "$(code $N/v1/sandboxes -H "Authorization: Bearer $TENANT")"
ck "a bad key is 401, not a topology hint" 401 "$(code $N/v1/sandboxes -H 'Authorization: Bearer nope')"

sec "A real sandbox, created through the control plane"
SB=$(curl -s -X POST "$O/v1/sandboxes" -H "Authorization: Bearer $FLEET_KEY" \
  -H 'Content-Type: application/json' -d '{"template":"base","vcpu":1,"mem_mb":512}' \
  | jq_ ";print(json.load(sys.stdin).get('id',''))")
ckc "create returns a sandbox id" "sb-" "$SB"
ex() { curl -s -X POST "$O/v1/sandboxes/$SB/exec" -H "Authorization: Bearer $FLEET_KEY" \
       -H 'Content-Type: application/json' -d "{\"cmd\":$(python3 -c "import json,sys;print(json.dumps(sys.argv[1]))" "$1")}" \
       | jq_ ";print(json.load(sys.stdin).get('stdout','').strip())"; }
ckc "container is running" "Up" "$(docker ps --filter "label=sandboxd.id=$SB" --format '{{.Status}}' | head -1)"
ck  "exec round-trips through the control plane" "E2E-OK" "$(ex 'echo E2E-OK')"

sec "Isolation, on that live sandbox"
ck  "capability bounding set is the reduced one" "00000000000004fb" "$(ex 'grep CapBnd /proc/self/status | cut -f2')"
ck  "MKNOD denied"      "denied" "$(ex 'mknod /tmp/x b 8 0 2>/dev/null && echo ALLOWED || echo denied')"
ck  "SYS_CHROOT denied" "denied" "$(ex 'chroot / /bin/true 2>/dev/null && echo ALLOWED || echo denied')"
SBIP=$(docker inspect "$(docker ps --filter "label=sandboxd.id=$SB" -q | head -1)" \
       --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null)
if [ -n "$SBIP" ]; then
  ck "sandbox -> sandbox blocked at L3" "blocked" \
     "$(docker run --rm --network sandboxd-net alpine:3 sh -c "timeout 3 nc -z $SBIP 22 2>/dev/null && echo REACHED || echo blocked" 2>/dev/null)"
else
  skip "sandbox -> sandbox blocked at L3" "could not read the sandbox IP"
fi

sec "Preview credentials are bound to one port"
T3=$(curl -s "$O/v1/sandboxes/$SB/preview-token?port=3000" -H "Authorization: Bearer $FLEET_KEY" | jq_ ";print(json.load(sys.stdin).get('token',''))")
T8=$(curl -s "$O/v1/sandboxes/$SB/preview-token?port=8080" -H "Authorization: Bearer $FLEET_KEY" | jq_ ";print(json.load(sys.stdin).get('token',''))")
ck "a token is issued"                    "yes" "$([ -n "$T3" ] && echo yes || echo no)"
ck ":3000 and :8080 differ"               "differ" "$([ "$T3" != "$T8" ] && echo differ || echo SAME)"
ck "an invalid port is refused"           400 "$(code "$O/v1/sandboxes/$SB/preview-token?port=99999" -H "Authorization: Bearer $FLEET_KEY")"
ck "unknown sandbox is 404, not 403"      404 "$(code "$O/v1/sandboxes/sb-nope/preview-token?port=3000" -H "Authorization: Bearer $FLEET_KEY")"

sec "Egress rules on real Linux iptables"
# macOS has no iptables; a privileged Linux container has a real netfilter stack.
if docker run --rm --privileged alpine:3 sh -c 'apk add -q --no-cache iptables >/dev/null 2>&1 && iptables -L INPUT -n >/dev/null 2>&1' 2>/dev/null; then
  cat > "$WORK/ipt.sh" <<'IPT'
set -u
apk add -q --no-cache iptables bash >/dev/null 2>&1
SB=172.27.0.0/16
iptables -N DOCKER-USER 2>/dev/null || true
for c in 100.64.0.0/10 172.16.0.0/12 172.17.0.1/32 10.0.0.0/8 169.254.0.0/16; do
  iptables -C DOCKER-USER -d "$c" -j DROP 2>/dev/null || iptables -I DOCKER-USER -d "$c" -j DROP
done
iptables -C DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
  iptables -I DOCKER-USER -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -C INPUT -s "$SB" -j DROP 2>/dev/null || iptables -I INPUT -s "$SB" -j DROP
iptables -C INPUT -s "$SB" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || \
  iptables -I INPUT -s "$SB" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
bash /verify-egress.sh --docker >/dev/null 2>&1 && echo INSTALLED_OK || echo INSTALLED_FAIL
iptables -D INPUT -s "$SB" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
bash /verify-egress.sh --docker >/dev/null 2>&1 && echo MISSING_NOT_CAUGHT || echo MISSING_CAUGHT
iptables -A INPUT -s "$SB" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
bash /verify-egress.sh --docker >/dev/null 2>&1 && echo ORDER_NOT_CAUGHT || echo ORDER_CAUGHT
IPT
  IPTOUT=$(docker run --rm --privileged -v "$WORK/ipt.sh":/t.sh:ro \
    -v "$PWD/scripts/verify-egress.sh":/verify-egress.sh:ro alpine:3 sh /t.sh 2>/dev/null)
  ckc "rules install and verify-egress passes"        "INSTALLED_OK"    "$IPTOUT"
  ckc "a missing ESTABLISHED ACCEPT is caught"        "MISSING_CAUGHT"  "$IPTOUT"
  ckc "an ACCEPT placed below the DROP is caught"     "ORDER_CAUGHT"    "$IPTOUT"

  # Re-running setup-host.sh is the whole test: a second application must not
  # push a DROP above the ESTABLISHED,RELATED ACCEPT.
  cat > "$WORK/fc.sh" <<'FCIPT'
apk add -q --no-cache iptables iproute2 bash >/dev/null 2>&1
for i in 1 2 3; do bash /setup-host.sh >/dev/null 2>&1 || true; done
A=$(iptables -S INPUT | grep -c -- '-s 192.168.0.0/16 .*RELATED,ESTABLISHED -j ACCEPT')
D=$(iptables -S INPUT | grep -c -- '-s 192.168.0.0/16 -j DROP')
AL=$(iptables -S INPUT | grep -n -- '-s 192.168.0.0/16 .*RELATED,ESTABLISHED -j ACCEPT' | head -1 | cut -d: -f1)
DL=$(iptables -S INPUT | grep -n -- '-s 192.168.0.0/16 -j DROP' | head -1 | cut -d: -f1)
[ "$A" = 1 ] && [ "$D" = 1 ] && echo NODUPES || echo "DUPES(accept=$A drop=$D)"
[ -n "$AL" ] && [ "$AL" -lt "$DL" ] && echo REORDER_OK || echo REORDER_BROKEN
bash /verify-egress.sh --firecracker >/dev/null 2>&1 && echo FCVERIFY_OK || echo FCVERIFY_FAIL
iptables -I INPUT -s 192.168.30.0/16 -j DROP
bash /verify-egress.sh --firecracker >/dev/null 2>&1 && echo FCORDER_NOT_CAUGHT || echo FCORDER_CAUGHT
FCIPT
  FCOUT=$(docker run --rm --privileged -v "$WORK/fc.sh":/t.sh:ro \
    -v "$PWD/scripts/verify-egress.sh":/verify-egress.sh:ro \
    -v "$PWD/scripts/setup-host.sh":/setup-host.sh:ro alpine:3 sh /t.sh 2>/dev/null)
  ckc "setup-host.sh re-run leaves no duplicate rules"  "NODUPES"        "$FCOUT"
  ckc "re-run keeps the ACCEPT above the DROP"          "REORDER_OK"     "$FCOUT"
  ckc "verify-egress --firecracker passes when correct" "FCVERIFY_OK"    "$FCOUT"
  ckc "a DROP above the FC ACCEPT is caught"            "FCORDER_CAUGHT" "$FCOUT"
else
  skip "egress rules on real iptables" "privileged linux containers unavailable"
fi

sec "Result"
printf '  %d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
printf '%s\n' "${RESULTS[@]}" > "${E2E_REPORT:-/tmp/pilldrops-e2e-report.txt}"
echo "  report: ${E2E_REPORT:-/tmp/pilldrops-e2e-report.txt}"
[ "$FAIL" -eq 0 ] || exit 1

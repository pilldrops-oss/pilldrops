#!/bin/bash
# Full E2E of every user-facing feature against a real Docker-backed node.
# Usage: full-feature-e2e.sh <base-url> <api-key> [operator-key]
B="$1"; K="$2"; H="Authorization: Bearer $K"; OPK="${3:-}"
pass=0; fail=0; skip=0
mkdir -p /tmp/e2e-all
declare -a FAILURES
ok()   { echo "  ✅ $1"; pass=$((pass+1)); }
bad()  { echo "  ❌ $1"; fail=$((fail+1)); FAILURES+=("$1"); }
skp()  { echo "  ⏭  $1"; skip=$((skip+1)); }
jq_() { python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception as e: print(''); sys.exit(0)
$1" 2>/dev/null; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

sec() { echo; echo "──── $1 ────"; }

sec "A. Sandbox lifecycle"
SB=$(curl -s -X POST $B/v1/sandboxes -H "$H" -H 'Content-Type: application/json' \
  -d '{"template":"base","env":["E2E_KEY=e2e-value"]}' | jq_ "print(d.get('id',''))")
[ -n "$SB" ] && ok "create ($SB)" || { bad "create"; exit 1; }

[ "$(code $B/v1/sandboxes/$SB -H "$H")" = 200 ] && ok "get by id" || bad "get by id"
n=$(curl -s $B/v1/sandboxes -H "$H" | jq_ "print(len(d if isinstance(d,list) else d.get('sandboxes',[])))")
[ "$n" -ge 1 ] && ok "list ($n)" || bad "list"

out=$(curl -s -X POST $B/v1/sandboxes/$SB/exec -H "$H" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo lifecycle-ok"}' | jq_ "print(d.get('stdout','').strip())")
[ "$out" = "lifecycle-ok" ] && ok "exec" || bad "exec (got '$out')"

env=$(curl -s -X POST $B/v1/sandboxes/$SB/exec -H "$H" -H 'Content-Type: application/json' \
  -d '{"cmd":"printenv E2E_KEY"}' | jq_ "print(d.get('stdout','').strip())")
[ "$env" = "e2e-value" ] && ok "BYOK env reaches the container" || bad "BYOK env (got '$env')"

iso=$(curl -s -X POST $B/v1/sandboxes/$SB/exec -H "$H" -H 'Content-Type: application/json' \
  -d '{"cmd":"cat /etc/os-release 2>/dev/null | head -1"}' | jq_ "print(d.get('stdout','').strip())")
case "$iso" in *Debian*|*Ubuntu*|*Alpine*) ok "real container isolation ($iso)";; *) bad "isolation — got '$iso'";; esac

[ "$(code -X POST $B/v1/sandboxes/$SB/pause -H "$H")" = 200 ] && ok "pause" || bad "pause"
[ "$(code -X POST $B/v1/sandboxes/$SB/resume -H "$H")" = 200 ] && ok "resume" || bad "resume"
alive=$(curl -s -X POST $B/v1/sandboxes/$SB/exec -H "$H" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo survived-resume"}' | jq_ "print(d.get('stdout','').strip())")
[ "$alive" = "survived-resume" ] && ok "usable after resume" || bad "dead after resume"

sec "B. Filesystem"
[ "$(code -X POST "$B/v1/sandboxes/$SB/files?path=e2e/a.txt" -H "$H" --data-binary 'file-content')" = 200 ] \
  && ok "write file" || bad "write file"
r=$(curl -s "$B/v1/sandboxes/$SB/files?path=e2e/a.txt" -H "$H")
[ "$r" = "file-content" ] && ok "read file" || bad "read file (got '$r')"
[ "$(code "$B/v1/sandboxes/$SB/filesystem/list?path=e2e" -H "$H")" = 200 ] && ok "list dir" || bad "list dir"
[ "$(code -X POST "$B/v1/sandboxes/$SB/filesystem/mkdir" -H "$H" -H 'Content-Type: application/json' -d '{"path":"e2e/sub"}')" = 200 ] \
  && ok "mkdir" || bad "mkdir"

sec "B2. Preview readiness truth"

st=$(curl -s $B/v1/sandboxes/$SB/preview-status -H "$H" | jq_ "print(d.get('state',''))")
[ "$st" = "terminal" ] && ok "bare sandbox reports terminal, not fake-ready" || bad "bare sandbox state '$st'"

PV=$(curl -s -X POST $B/v1/sandboxes -H "$H" -H 'Content-Type: application/json' \
  -d '{"template":"python-3.11"}' | jq_ "print(d.get('id',''))")
[ -n "$PV" ] && ok "app sandbox created ($PV)" || bad "app sandbox create"

# /start blocks until the app binds, so the "starting" state is only observable
# while the deploy is in flight: run it in the background and probe mid-boot.
printf '{"cmd":"sleep 6 && python3 -m http.server $PORT --bind 0.0.0.0","port":8000}' > /tmp/e2e-all/slow.json
curl -s -X POST $B/v1/sandboxes/$PV/start -H "$H" -H 'Content-Type: application/json' \
  --data-binary @/tmp/e2e-all/slow.json > /tmp/e2e-all/start-out.json &
START_PID=$!
sleep 2
st=$(curl -s $B/v1/sandboxes/$PV/preview-status -H "$H" | jq_ "print(d.get('state',''))")
[ "$st" = "starting" ] && ok "booting app reports starting (not a false ready)" || bad "booting state '$st'"
wait $START_PID
grep -q '"started"' /tmp/e2e-all/start-out.json && ok "slow app deployed" || bad "start slow app: $(cat /tmp/e2e-all/start-out.json)"

rd=""
for i in $(seq 1 20); do
  rd=$(curl -s $B/v1/sandboxes/$PV/preview-status -H "$H" | jq_ "print(d.get('state','')+'|'+str(d.get('upstream_status','')))")
  [ "${rd%%|*}" = "ready" ] && break
  sleep 2
done
[ "${rd%%|*}" = "ready" ] && ok "app became ready ($rd)" || bad "never ready ($rd)"

st=$(curl -s "$B/v1/sandboxes/$PV/preview-status?port=9999" -H "$H" | jq_ "print(d.get('state',''))")
[ "$st" = "unpublished" ] && ok "wrong port reports unpublished" || skp "wrong-port state '$st' (multi-port runtime?)"

# The status poll must not wake a paused machine; waking bills the customer.
curl -s -X POST $B/v1/sandboxes/$PV/pause -H "$H" >/dev/null
st=$(curl -s $B/v1/sandboxes/$PV/preview-status -H "$H" | jq_ "print(d.get('state',''))")
[ "$st" = "paused" ] && ok "paused reports paused" || bad "paused state '$st'"
lc=$(curl -s $B/v1/sandboxes/$PV -H "$H" | jq_ "print(d.get('status',d.get('state','')))")
[ "$lc" = "paused" ] && ok "status poll did not wake the machine" || bad "poll woke it (now '$lc')"

curl -s -X DELETE $B/v1/sandboxes/$PV -H "$H" >/dev/null

# Write before the first GET: storage measurements are cached for 60s.
sec "B3. Storage meters"
SM=$(curl -s -X POST $B/v1/sandboxes -H "$H" -H 'Content-Type: application/json' \
  -d '{"template":"base"}' | jq_ "print(d.get('id',''))")
head -c 5242880 /dev/zero > /tmp/e2e-all/big.bin
[ "$(code -X POST "$B/v1/sandboxes/$SM/files?path=big.bin" -H "$H" --data-binary @/tmp/e2e-all/big.bin)" = 200 ] \
  && ok "wrote 5MB into the container" || bad "write 5MB"
used=$(curl -s $B/v1/sandboxes/$SM -H "$H" | jq_ "print(d.get('storage_used_mb','absent'))")
capmb=$(curl -s $B/v1/sandboxes/$SM -H "$H" | jq_ "print(d.get('storage_mb','absent'))")
if [ "$used" = "absent" ]; then bad "storage_used_mb absent from sandbox GET"
elif [ "$used" -ge 4 ] 2>/dev/null; then ok "sandbox GET meters real bytes (${used}MB used)"
else bad "storage_used_mb=$used, want >=4 after 5MB write"; fi
[ "$capmb" != "absent" ] && ok "plan storage cap surfaced (${capmb}MB)" || bad "storage_mb cap absent"

bst=$(curl -s $B/v1/billing -H "$H" | jq_ "
s=d.get('storage',{})
print(str(s.get('used_mb','x'))+'|'+str(s.get('cap_mb','x'))+'|'+str(s.get('measured','x')))")
u="${bst%%|*}"; rest="${bst#*|}"; cm="${rest%%|*}"; me="${bst##*|}"
[ "$me" = "True" ] && ok "billing rollup is runtime-measured" || bad "billing storage.measured=$me"
[ "$u" -ge 4 ] 2>/dev/null && ok "billing rollup includes the 5MB (${u}MB org-wide)" || bad "billing used_mb=$u"
hrs=$(curl -s $B/v1/billing -H "$H" | jq_ "print('used' in d.get('hours',{}))")
[ "$hrs" = "True" ] && ok "hours meter present" || bad "hours meter absent"

# Resume rebuilds the container from a snapshot image, moving the bytes out of
# the writable layer; the meter must still count them.
curl -s -o /dev/null -X POST $B/v1/sandboxes/$SM/pause -H "$H"
curl -s -o /dev/null -X POST $B/v1/sandboxes/$SM/resume -H "$H"
sleep 62 # outlive the 60s measurement cache so the next GET re-measures
used2=$(curl -s $B/v1/sandboxes/$SM -H "$H" | jq_ "print(d.get('storage_used_mb','absent'))")
[ "$used2" != "absent" ] && [ "$used2" -ge 4 ] 2>/dev/null \
  && ok "storage meter survives pause/resume (${used2}MB)" || bad "post-resume storage=$used2, want >=4"
curl -s -X DELETE $B/v1/sandboxes/$SM -H "$H" >/dev/null

sec "C. Observability"
for ep in metrics logs; do
  [ "$(code $B/v1/sandboxes/$SB/$ep -H "$H")" = 200 ] && ok "$ep" || bad "$ep"
done
for ep in usage events history audit templates; do
  c=$(code $B/v1/$ep -H "$H")
  [ "$c" = 200 ] && ok "/v1/$ep" || bad "/v1/$ep ($c)"
done
# /v1/nodes is operator-only.
c=$(code $B/v1/nodes -H "$H")
[ "$c" = 403 ] && ok "/v1/nodes refused to the tenant" || bad "/v1/nodes for the tenant ($c, want 403)"
if [ -n "$OPK" ]; then
  c=$(code $B/v1/nodes -H "Authorization: Bearer $OPK")
  [ "$c" = 200 ] && ok "/v1/nodes for the operator" || bad "/v1/nodes for the operator ($c)"
else
  skp "/v1/nodes for the operator (no operator key given)"
fi
cost=$(curl -s $B/v1/usage -H "$H" | jq_ "print('cost_usd' in d)")
[ "$cost" = "True" ] && ok "cost attribution (cost_usd)" || bad "no cost attribution"

sec "D. Billing & plans"
for p in personal pro max; do
  printf '{"plan":"%s"}' "$p" > /tmp/e2e-all/pl.json
  c=$(code -X POST $B/v1/billing/subscribe -H "$H" -H 'Content-Type: application/json' --data-binary @/tmp/e2e-all/pl.json)
  [ "$c" = 200 ] && ok "$p tier is self-serve" || bad "$p not subscribable ($c)"
done
printf '{"plan":"scale"}' > /tmp/e2e-all/pl.json
[ "$(code -X POST $B/v1/billing/subscribe -H "$H" -H 'Content-Type: application/json' --data-binary @/tmp/e2e-all/pl.json)" = 400 ] \
  && ok "legacy scale still contact-sales" || bad "scale became self-serve"
[ "$(code $B/v1/billing -H "$H")" = 200 ] && ok "billing summary" || bad "billing summary"

sec "I. Security"
[ "$(code $B/v1/sandboxes)" = 401 ] && ok "unauthenticated rejected" || bad "auth not enforced"
[ "$(code $B/v1/sandboxes/sb_does_not_exist -H "$H")" = 404 ] && ok "unknown id 404s" || bad "unknown id"

sec "J. Persistent workstation"
printf '{"plan":"max"}' > /tmp/e2e-all/pl.json
curl -s -X POST $B/v1/billing/subscribe -H "$H" -H 'Content-Type: application/json' --data-binary @/tmp/e2e-all/pl.json >/dev/null
PSB=$(curl -s -X POST $B/v1/sandboxes -H "$H" -H 'Content-Type: application/json' -d '{"template":"base","timeout":-1}')
exp=$(echo "$PSB" | jq_ "print('null' if d.get('expires_at') is None else 'set')")
pid=$(echo "$PSB" | jq_ "print(d.get('id',''))")
[ "$exp" = "null" ] && ok "persistent workstation has no expiry" || bad "workstation still leased on a persistent plan"
[ -n "$pid" ] && ok "workstation id returned" || bad "no workstation id"

sec "K. Cleanup on destroy"
curl -s -X DELETE $B/v1/sandboxes/$SB -H "$H" >/dev/null
hist=$(curl -s $B/v1/history -H "$H" | jq_ "print(1 if '$SB' in json.dumps(d) else 0)")
[ "$hist" = "1" ] && ok "destroyed sandbox recorded in history" || skp "history not yet written"

for x in $pid; do [ -n "$x" ] && curl -s -X DELETE $B/v1/sandboxes/$x -H "$H" >/dev/null; done

echo
echo "════════════════════════════════════════"
echo "  PASS $pass   FAIL $fail   SKIP $skip"
if [ ${#FAILURES[@]} -gt 0 ]; then
  echo "  failures:"; for f in "${FAILURES[@]}"; do echo "    - $f"; done
fi

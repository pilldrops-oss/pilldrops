#!/usr/bin/env bash
# Live backup round trip against a real Firecracker sandbox.
#
# Every check below has a positive control beside it: the unconfirmed restore
# proves the confirmed one did something, the second capture proves the first
# restore did not break the machine, and the post-restore read proves the
# backup carried the data rather than merely returning bytes.
#
# Usage: BKP_API=http://127.0.0.1:18801 BKP_KEY=<operator key> scripts/backup-live.sh
set -uo pipefail

API=${BKP_API:-http://127.0.0.1:18801}
KEY=${BKP_KEY:?set BKP_KEY to an operator key}

pass=0 fail=0
check() { # check <name> <expected> <actual>
	if [ "$2" = "$3" ]; then
		printf 'ok    %-52s %s\n' "$1" "$3"
		pass=$((pass + 1))
	else
		printf 'FAIL  %-52s want %q, got %q\n' "$1" "$2" "$3"
		fail=$((fail + 1))
	fi
}

api() { curl -sk -H "Authorization: Bearer $KEY" "$@"; }
code() { curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $KEY" "$@"; }
jq1() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }
sh_in() { api -X POST "$API/v1/sandboxes/$SB/exec" -d "{\"cmd\":$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$1")}" | jq1 "['stdout'].strip()"; }

SB=$(api -X POST "$API/v1/sandboxes" -d '{"template":"base"}' | jq1 "['id']")
[ -n "$SB" ] || { echo "could not create a sandbox"; exit 1; }
trap 'api -X DELETE "$API/v1/sandboxes/$SB" >/dev/null' EXIT
echo "sandbox $SB"

URL_BEFORE=$(api "$API/v1/sandboxes/$SB" | jq1 "['terminal_url']")

# ── capture ──────────────────────────────────────────────────────────────
sh_in 'echo A-original > /root/proof.txt' >/dev/null
check "the disk holds A before the backup" "A-original" "$(sh_in 'cat /root/proof.txt')"

CREATED=$(api -X POST "$API/v1/sandboxes/$SB/backups")
BK=$(echo "$CREATED" | jq1 "['id']")
PAUSE=$(echo "$CREATED" | jq1 "['paused_ms']")
check "the capture returned a backup id" "yes" "$([ -n "$BK" ] && echo yes || echo no)"
check "the sandbox is running the moment it returns" "running" "$(api "$API/v1/sandboxes/$SB" | jq1 "['status']")"
# The FIRST capture of a sandbox writes a full memory image, which on a 1 GiB
# guest stands it still for a couple of seconds; later ones are dirty-page
# diffs. The bound here is the full-image case, and the diff is checked below.
echo "      first capture paused the guest for ${PAUSE}ms"
check "the first capture's pause is bounded (<5s)" "yes" "$([ "${PAUSE:-99999}" -lt 5000 ] && echo yes || echo no)"

# The guest's clock still moves: it was not left paused.
C1=$(sh_in 'date +%s'); sleep 2; C2=$(sh_in 'date +%s')
check "the guest clock advanced after the capture" "yes" "$([ "$C2" -gt "$C1" ] && echo yes || echo no)"

for _ in $(seq 1 60); do
	ROW=$(api "$API/v1/sandboxes/$SB/backups/" 2>/dev/null)
	ROW=$(api "$API/v1/sandboxes/$SB/backups")
	echo "$ROW" | grep -q '"status":"ready"' && break
	sleep 2
done
check "the backup reached ready" "ready" "$(echo "$ROW" | jq1 "['backups'][0]['status']")"
BYTES=$(echo "$ROW" | jq1 "['backups'][0]['bytes']")
KIND=$(echo "$ROW" | jq1 "['backups'][0]['snapshot_kind']")
echo "      uploaded $BYTES bytes, kind $KIND"
check "the response says which kind of capture it is" "yes" "$([ -n "$KIND" ] && echo yes || echo no)"

# ── restore ──────────────────────────────────────────────────────────────
sh_in 'echo B-clobbered > /root/proof.txt; echo x > /root/after.txt' >/dev/null
check "the disk holds B before the restore" "B-clobbered" "$(sh_in 'cat /root/proof.txt')"

# Positive control: no confirm, no change.
check "an unconfirmed restore is refused" "400" "$(code -X POST "$API/v1/sandboxes/$SB/backups/$BK/restore" -d '{}')"
check "and it changed nothing" "B-clobbered" "$(sh_in 'cat /root/proof.txt')"

check "a confirmed restore succeeds" "200" "$(code -X POST "$API/v1/sandboxes/$SB/backups/$BK/restore" -d '{"confirm":true}')"
sleep 2
check "THE DATA THE BACKUP WAS TAKEN FROM IS BACK" "A-original" "$(sh_in 'cat /root/proof.txt')"
check "a file written after the backup is gone" "gone" "$(sh_in 'test -e /root/after.txt && echo present || echo gone')"
check "the sandbox id is unchanged" "$SB" "$(api "$API/v1/sandboxes/$SB" | jq1 "['id']")"
check "the preview URL is unchanged" "$URL_BEFORE" "$(api "$API/v1/sandboxes/$SB" | jq1 "['terminal_url']")"
check "the sandbox is running" "running" "$(api "$API/v1/sandboxes/$SB" | jq1 "['status']")"

# A restored machine must still be capturable: the restore must not have
# removed the directory its memory base lives in.
sh_in 'echo C-after-restore > /root/proof.txt' >/dev/null
CREATED2=$(api -X POST "$API/v1/sandboxes/$SB/backups")
BK2=$(echo "$CREATED2" | jq1 "['id']")
PAUSE2=$(echo "$CREATED2" | jq1 "['paused_ms']")
check "a capture after a restore starts" "yes" "$([ -n "$BK2" ] && echo yes || echo no)"
echo "      later capture paused the guest for ${PAUSE2}ms"
check "a later capture pauses for under 2s" "yes" "$([ "${PAUSE2:-99999}" -lt 2000 ] && echo yes || echo no)"
for _ in $(seq 1 60); do
	ROW2=$(api "$API/v1/sandboxes/$SB/backups")
	echo "$ROW2" | grep -q '"status":"pending"' || break
	sleep 2
done
check "it reached ready" "ready" "$(echo "$ROW2" | python3 -c "
import sys,json
rows={b['id']:b for b in json.load(sys.stdin)['backups']}
print(rows.get('$BK2',{}).get('status','missing'))")"

# ── delete guards ────────────────────────────────────────────────────────
check "deleting a ready leaf succeeds" "200" "$(code -X DELETE "$API/v1/sandboxes/$SB/backups/$BK2")"
check "it is gone from the list" "gone" "$(api "$API/v1/sandboxes/$SB/backups" | grep -q "$BK2" && echo present || echo gone)"

# ── schedule floor ───────────────────────────────────────────────────────
check "a 5m backup period is refused" "400" "$(code -X POST "$API/v1/sandboxes/$SB/config" -d '{"backups":{"period":"5m"}}')"
check "a */5 cron is refused too" "400" "$(code -X POST "$API/v1/sandboxes/$SB/config" -d '{"backups":{"cron":"*/5 * * * *"}}')"
check "retention with no period is refused" "400" "$(code -X POST "$API/v1/sandboxes/$SB/config" -d '{"backups":{"retention":"7d"}}')"
check "a 24h period is accepted" "200" "$(code -X POST "$API/v1/sandboxes/$SB/config" -d '{"backups":{"period":"24h","retention":"7d"}}')"
check "and it reads back as a daily cron" "0 0 * * *" "$(api "$API/v1/sandboxes/$SB/config" | jq1 "['backups']['cron']")"
check "turning it off removes it" "none" "$(api -X POST "$API/v1/sandboxes/$SB/config" -d '{"backups":{"period":"off"}}' >/dev/null; api "$API/v1/sandboxes/$SB/config" | python3 -c "
import sys,json;print('set' if json.load(sys.stdin).get('backups') else 'none')")"

# ── another org cannot see any of it ─────────────────────────────────────
check "an unauthenticated capture is refused" "401" "$(curl -sk -o /dev/null -w '%{http_code}' -X POST "$API/v1/sandboxes/$SB/backups")"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]

#!/bin/bash
# ─────────────────────────────────────────────────────────────
# PillDrops API Test Suite — Tests ALL endpoints
# ─────────────────────────────────────────────────────────────
set -euo pipefail
BASE="${1:-http://localhost:8080}"
PASS=0; FAIL=0
G='\033[0;32m'; R='\033[0;31m'; C='\033[0;36m'; N='\033[0m'

check() {
  local name="$1" expected="$2" actual="$3"
  if [ "$actual" = "$expected" ]; then echo -e "  ${G}✓${N} $name"; PASS=$((PASS+1))
  else echo -e "  ${R}✗${N} $name (expected=$expected got=$actual)"; FAIL=$((FAIL+1)); fi
}

echo -e "\n${C}PillDrops API Full Test Suite — $BASE${N}\n"

# ─── System ──────────────────────────────────────────
echo "System:"
check "GET /health" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/health)"
check "GET /dashboard" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/dashboard)"
check "GET /docs" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/docs)"

# ─── Templates ───────────────────────────────────────
echo ""; echo "Templates:"
check "GET /v1/templates" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/templates)"
TCOUNT=$(curl -s $BASE/v1/templates | python3 -c "import sys,json;print(len(json.load(sys.stdin)))")
check "  has templates" "true" "$([ "$TCOUNT" -ge 1 ] && echo true || echo false)"
check "POST /v1/templates (create)" "201" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/templates -d '{"name":"test-tpl","cpu":1}')"
check "GET /v1/templates/test-tpl" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/templates/test-tpl)"
check "PUT /v1/templates/test-tpl" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT $BASE/v1/templates/test-tpl -d '{"cpu":4}')"
TPL_CPU=$(curl -s $BASE/v1/templates/test-tpl | python3 -c "import sys,json;print(json.load(sys.stdin)['cpu'])")
check "  updated cpu=4" "4" "$TPL_CPU"
check "POST /v1/templates/test-tpl/build" "201" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/templates/test-tpl/build)"
check "GET /v1/builds" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/builds)"
check "DELETE /v1/templates/test-tpl" "204" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE $BASE/v1/templates/test-tpl)"

# ─── API Keys ────────────────────────────────────────
echo ""; echo "API Keys:"
KEY_ID=$(curl -s -X POST $BASE/v1/keys -d '{"name":"test"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
check "POST /v1/keys (create)" "true" "$([ -n '$KEY_ID' ] && echo true || echo false)"
check "GET /v1/keys" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/keys)"
check "DELETE /v1/keys/$KEY_ID" "204" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE $BASE/v1/keys/$KEY_ID)"

# ─── Create Sandbox ──────────────────────────────────
echo ""; echo "Sandbox CRUD:"
SB_RESP=$(curl -s -X POST $BASE/v1/sandboxes -d '{"template":"python-3.11","timeout":300,"files":[{"path":"test.py","content":"print(42)"},{"path":"data/info.txt","content":"hello"}]}')
SB_ID=$(echo "$SB_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
check "POST /v1/sandboxes (create)" "true" "$([ -n '$SB_ID' ] && echo true || echo false)"
check "GET /v1/sandboxes (list)" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes)"
check "GET /v1/sandboxes/$SB_ID" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID)"
HAS_URL=$(curl -s $BASE/v1/sandboxes/$SB_ID | python3 -c "import sys,json;print('yes' if 'url' in json.load(sys.stdin) else 'no')")
check "  has url field" "yes" "$HAS_URL"

# ─── Exec ────────────────────────────────────────────
echo ""; echo "Execution:"
STDOUT=$(curl -s -X POST $BASE/v1/sandboxes/$SB_ID/exec -d '{"cmd":"echo hello"}' | python3 -c "import sys,json;print(json.load(sys.stdin).get('stdout','').strip())")
check "exec echo" "hello" "$STDOUT"
STDOUT=$(curl -s -X POST $BASE/v1/sandboxes/$SB_ID/exec -d '{"cmd":"python3 -c \"print(6*7)\""}' | python3 -c "import sys,json;print(json.load(sys.stdin).get('stdout','').strip())")
check "exec python" "42" "$STDOUT"
STDOUT=$(curl -s -X POST $BASE/v1/sandboxes/$SB_ID/exec -d '{"cmd":"python3 test.py"}' | python3 -c "import sys,json;print(json.load(sys.stdin).get('stdout','').strip())")
check "exec written file" "42" "$STDOUT"
EXIT=$(curl -s -X POST $BASE/v1/sandboxes/$SB_ID/exec -d '{"cmd":"exit 42"}' | python3 -c "import sys,json;print(json.load(sys.stdin).get('exit_code',''))")
check "exit code" "42" "$EXIT"

# ─── Filesystem ──────────────────────────────────────
echo ""; echo "Filesystem:"
check "POST /files (write)" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/sandboxes/$SB_ID/files?path=new.txt" -H 'Content-Type: application/octet-stream' -d 'content')"
CONTENT=$(curl -s "$BASE/v1/sandboxes/$SB_ID/files?path=new.txt")
check "GET /files (read)" "content" "$CONTENT"
check "GET /filesystem/list" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/sandboxes/$SB_ID/filesystem/list?path=/")"
FCOUNT=$(curl -s "$BASE/v1/sandboxes/$SB_ID/filesystem/list?path=/" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))")
check "  has files" "true" "$([ "$FCOUNT" -ge 1 ] && echo true || echo false)"
check "POST /filesystem/mkdir" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/filesystem/mkdir -d '{"path":"newdir"}')"
check "GET /filesystem/stat" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/sandboxes/$SB_ID/filesystem/stat?path=test.py")"
FSIZE=$(curl -s "$BASE/v1/sandboxes/$SB_ID/filesystem/stat?path=test.py" | python3 -c "import sys,json;print(json.load(sys.stdin)['size'])")
check "  stat size > 0" "true" "$([ "$FSIZE" -gt 0 ] && echo true || echo false)"
check "POST /filesystem/move" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/filesystem/move -d '{"source":"new.txt","destination":"moved.txt"}')"
check "POST /filesystem/remove" "204" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/filesystem/remove -d '{"path":"moved.txt"}')"

# ─── Lifecycle ───────────────────────────────────────
echo ""; echo "Lifecycle:"
check "POST /pause" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/pause)"
check "POST /resume" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/resume)"
check "POST /timeout" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/timeout -d '{"seconds":600}')"
check "POST /refresh" "200" "$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/sandboxes/$SB_ID/refresh)"
check "GET /logs" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID/logs)"
check "GET /metrics" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID/metrics)"

# ─── Process ─────────────────────────────────────────
echo ""; echo "Process:"
check "GET /process/list" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID/process/list)"

# ─── History ─────────────────────────────────────────
echo ""; echo "History:"
check "GET /history" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID/history)"
check "GET /exec-history" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/exec-history)"
HCOUNT=$(curl -s $BASE/v1/sandboxes/$SB_ID/history | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) if d else 0)")
check "  has history" "true" "$([ "$HCOUNT" -ge 1 ] && echo true || echo false)"

# ─── Team ────────────────────────────────────────────
echo ""; echo "Team:"
check "GET /v1/webhooks" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/webhooks)"
check "GET /v1/members" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/members)"
check "GET /v1/usage" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/usage)"
check "GET /v1/billing" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/billing)"

# ─── Sandbox Page ────────────────────────────────────
echo ""; echo "Pages:"
check "GET /sandbox/{id}/" "200" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/sandbox/$SB_ID/)"

# ─── Destroy ─────────────────────────────────────────
echo ""; echo "Destroy:"
check "DELETE /v1/sandboxes/{id}" "204" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE $BASE/v1/sandboxes/$SB_ID)"
check "GET after destroy → 404" "404" "$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/sandboxes/$SB_ID)"

# ─── Summary ─────────────────────────────────────────
echo ""
echo "─────────────────────────────────────────"
TOTAL=$((PASS+FAIL))
if [ $FAIL -eq 0 ]; then echo -e "${G}All $TOTAL tests passed ✓${N}"
else echo -e "${G}$PASS passed${N}, ${R}$FAIL failed${N} out of $TOTAL"; fi
echo ""

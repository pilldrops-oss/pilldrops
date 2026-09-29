#!/usr/bin/env bash
# bench-gate.sh: fail when a latency result regressed against the most recent
# committed baseline for the same runtime, cache state and runner class. The
# comparison is relative; an absolute threshold is wrong on the next runner.
#
# Exit codes:
#   0  compared, within tolerance
#   1  compared, regressed beyond tolerance
#   2  usage or malformed input
#   3  no matching baseline, never reported as a pass
set -euo pipefail

TOLERANCE="${BENCH_GATE_TOLERANCE:-20}"   # percent
METRIC="ready_ms"
BASE_DIR="docs/benchmarks"
RESULT=""

usage() {
  cat <<'EOF'
usage: bench-gate.sh --result <file.json> [--baselines <dir>] [--tolerance <pct>]

  --result F      the new result document to judge
  --baselines D   root holding <date>/<runtime>-<cache>.json (default docs/benchmarks)
  --tolerance P   percent p95 regression that fails (default 20, or $BENCH_GATE_TOLERANCE)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --result)    RESULT="${2:-}"; shift 2 ;;
    --baselines) BASE_DIR="${2:-}"; shift 2 ;;
    --tolerance) TOLERANCE="${2:-}"; shift 2 ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

[ -n "$RESULT" ] || { usage; exit 2; }
[ -f "$RESULT" ] || { echo "no such result: $RESULT" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

field() { jq -er "$1" "$2" 2>/dev/null; }

RUNTIME=$(field '.config.runtime' "$RESULT")   || { echo "$RESULT: no .config.runtime" >&2; exit 2; }
CACHE=$(field '.config.cache_state' "$RESULT") || { echo "$RESULT: no .config.cache_state" >&2; exit 2; }
CLASS=$(field '.runner.class' "$RESULT")       || { echo "$RESULT: no .runner.class" >&2; exit 2; }
NEW=$(field ".summary.\"$METRIC\".p95" "$RESULT") || { echo "$RESULT: no $METRIC p95" >&2; exit 2; }
NEW_N=$(field ".summary.\"$METRIC\".n" "$RESULT") || NEW_N=0
FAILURES=$(jq -er '.failures' "$RESULT" 2>/dev/null || echo 0)

echo "gate: $RUNTIME/$CACHE on $CLASS — new p95 $METRIC = ${NEW}ms (n=$NEW_N, failures=$FAILURES)"

# summarize() returns zeroes for an empty set, and 0 would beat every baseline.
if [ "${NEW_N:-0}" -eq 0 ]; then
  echo "FAIL: the new result has no successful samples — every sample failed." >&2
  exit 1
fi

# Same filename in any dated directory except the result's own; sorted so the most recent wins.
RESULT_ABS=$(cd "$(dirname "$RESULT")" && pwd)/$(basename "$RESULT")
BEST=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  ABS=$(cd "$(dirname "$f")" && pwd)/$(basename "$f")
  [ "$ABS" = "$RESULT_ABS" ] && continue
  [ "$(field '.runner.class' "$f" || echo)" = "$CLASS" ] || continue
  BEST="$f"
done < <(find "$BASE_DIR" -type f -name "${RUNTIME}-${CACHE}.json" 2>/dev/null | sort)

if [ -z "$BEST" ]; then
  echo "NO BASELINE for $RUNTIME/$CACHE on $CLASS under $BASE_DIR." >&2
  echo "Nothing was compared. This is not a pass — commit a baseline for this" >&2
  echo "configuration before relying on the gate." >&2
  exit 3
fi

OLD=$(field ".summary.\"$METRIC\".p95" "$BEST")   || { echo "$BEST: no $METRIC p95" >&2; exit 2; }
OLD_SHA=$(field '.git_sha' "$BEST" || echo unknown)
NEW_SHA=$(field '.git_sha' "$RESULT" || echo unknown)

DELTA=$(awk -v n="$NEW" -v o="$OLD" 'BEGIN{ if (o==0) print 0; else printf "%.2f", 100*(n-o)/o }')
echo "      baseline $BEST (${OLD_SHA:0:12}) p95 = ${OLD}ms"
echo "      new      ${NEW_SHA:0:12}          p95 = ${NEW}ms   delta ${DELTA}%"

REGRESSED=$(awk -v d="$DELTA" -v t="$TOLERANCE" 'BEGIN{ print (d > t) ? 1 : 0 }')
if [ "$REGRESSED" = "1" ]; then
  echo "FAIL: $RUNTIME/$CACHE p95 regressed ${DELTA}% (tolerance ${TOLERANCE}%)" >&2
  echo "      ${OLD}ms -> ${NEW}ms, ${OLD_SHA:0:12} -> ${NEW_SHA:0:12}" >&2
  exit 1
fi
echo "OK    within tolerance (${TOLERANCE}%)"

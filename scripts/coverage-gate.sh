#!/usr/bin/env bash
# coverage-gate.sh: a ratchet on the packages where a coverage regression is a
# security regression (credentials, authorization, tenant limits). Each floor
# sits a few points under the measured value; raise it after adding coverage.
#
# Usage: ./scripts/coverage-gate.sh [-update]
#   -update  print floors from today's measurement for pasting back in; does
#            not edit this file.
set -euo pipefail

# package<TAB>floor
# internal/runtime/docker boots real containers, so its floor has extra slack. internal/cluster
# needs SANDBOXD_TEST_REDIS_URL (CI sets it) or its Redis halves skip and it reports below floor.
FLOORS=$(cat <<'EOF'
internal/auth/sbtoken	95.0
internal/auth/orgassert	95.0
internal/proxy	90.0
internal/auth/previewtoken	90.0
internal/cluster	77.0
internal/orchestrator	68.0
internal/httpapi	60.0
internal/store	60.0
internal/runtime/docker	48.0
EOF
)

update=false
[ "${1:-}" = "-update" ] && update=true

pkgs=$(echo "$FLOORS" | cut -f1 | sed 's#^#./#' | tr '\n' ' ')

# One `go test` invocation; separate ones re-link the world each time.
out=$(go test $pkgs -cover -count=1 -timeout 30m 2>&1) || {
  echo "$out"
  echo "FAIL: tests did not pass, so coverage was not measured." >&2
  exit 1
}

fail=0
printf "  %-32s %8s %8s\n" "package" "actual" "floor"
while IFS=$'\t' read -r pkg floor; do
  [ -z "$pkg" ] && continue
  # "ok  <module>/<pkg>  1.2s  coverage: 93.1% of statements"
  line=$(echo "$out" | grep -E "/${pkg}[[:space:]]" || true)
  actual=$(echo "$line" | sed -nE 's/.*coverage: ([0-9.]+)% of statements.*/\1/p' | head -1)
  if [ -z "$actual" ]; then
    printf "  %-32s %8s %8s   NO COVERAGE REPORTED\n" "$pkg" "-" "$floor"
    fail=1
    continue
  fi
  if awk "BEGIN{exit !($actual < $floor)}"; then
    printf "  %-32s %8s %8s   BELOW FLOOR\n" "$pkg" "$actual" "$floor"
    fail=1
  else
    printf "  %-32s %8s %8s\n" "$pkg" "$actual" "$floor"
  fi
done <<< "$FLOORS"

if $update; then
  echo
  echo "Floors from today's measurement (2 points of headroom), to paste above:"
  while IFS=$'\t' read -r pkg _; do
    [ -z "$pkg" ] && continue
    line=$(echo "$out" | grep -E "/${pkg}[[:space:]]" || true)
    actual=$(echo "$line" | sed -nE 's/.*coverage: ([0-9.]+)% of statements.*/\1/p' | head -1)
    [ -z "$actual" ] && continue
    printf "%s\t%s\n" "$pkg" "$(awk "BEGIN{f=$actual-2; if(f<0)f=0; printf \"%.1f\", f}")"
  done <<< "$FLOORS"
fi

if [ "$fail" -ne 0 ]; then
  echo
  echo "A gated package lost coverage. If a test was deleted on purpose, say so and" >&2
  echo "lower the floor in this file deliberately — do not lower it to make CI green." >&2
  exit 1
fi
echo
echo "Coverage floors held."

#!/usr/bin/env bash
# no-silent-skips.sh: fail when a package has _test.go files but a build
# constraint selects none of them on this platform. `go test` prints
# "[no test files]" for such a package and reports success.
#
# Usage:
#   scripts/no-silent-skips.sh              # this platform
#   GOOS=linux scripts/no-silent-skips.sh   # what CI will run
set -euo pipefail

cd "$(dirname "$0")/.."

# Packages deliberately constrained to one platform warn instead of failing.
# Add an entry only if CI still runs the package somewhere.
known_constrained="internal/vm"

fail=0
found=0
warned=0

# `go list` reports which test files the current build constraints select.
while IFS='|' read -r dir selected; do
  [ -n "$dir" ] || continue
  on_disk=$(find "$dir" -maxdepth 1 -name '*_test.go' 2>/dev/null | wc -l | tr -d ' ')
  [ "$on_disk" != "0" ] || continue
  found=$((found + 1))
  if [ "$selected" = "0" ]; then
    case " $known_constrained " in
      *" $dir "*)
        printf '  not run here  %-41s %s test file(s), constrained off %s — runs in Linux CI\n' \
          "$dir" "$on_disk" "$(go env GOOS)"
        warned=$((warned + 1))
        ;;
      *)
        printf '  SILENT SKIP   %-41s %s test file(s) on disk, 0 selected for %s/%s\n' \
          "$dir" "$on_disk" "$(go env GOOS)" "$(go env GOARCH)"
        fail=1
        ;;
    esac
  fi
done < <(
  go list -f '{{.Dir}}|{{len .TestGoFiles}}{{len .XTestGoFiles}}' ./... 2>/dev/null |
    sed "s#^$PWD/##" |
    awk -F'|' '{ n = $2; gsub(/[^0-9]/, "", n); print $1 "|" ($2 == "00" ? 0 : 1) }'
)

if [ "$fail" -ne 0 ]; then
  echo
  echo "A package with test files ran none of them on this platform."
  echo "Run those tests where they are constrained to run — for internal/vm that is Linux:"
  echo
  echo "  docker run --rm -v \"\$PWD\":/src -w /src golang:1.25 go test ./internal/vm/"
  echo
  echo "Do NOT filter \"no test files\" out of a test run's output. That line is the"
  echo "only thing that distinguishes a tested package from an untested one."
  exit 1
fi

if [ "$warned" -ne 0 ]; then
  echo
  echo "$warned package(s) above are constrained off $(go env GOOS) and did NOT run here."
  echo "Run them where they are constrained to run before trusting a green suite:"
  echo
  echo "  docker run --rm -v \"\$PWD\":/src -w /src golang:1.25 go test ./internal/vm/"
  echo
fi
echo "no silent skips: $found package(s) with test files, $((found - warned)) selected on $(go env GOOS)/$(go env GOARCH)."

#!/usr/bin/env bash
# check.sh: run locally what CI would run.
#
# The Go suite and the scanners run in a Linux container, not on the host:
# internal/vm is //go:build linux, and gosec/staticcheck only see what the
# current GOOS compiles, so a macOS run reports a false clean.
#
# A check that could not run is reported as SKIP, never as a pass. --strict
# turns any skip into a failure.
#
# Usage: scripts/check.sh [--strict] [--only=NAME]
#   CHECK_HOST=user@box   run the Linux half on a real box over ssh
#   CHECK_TIMEOUT=secs    per-check ceiling (default 900)
set -uo pipefail
cd "$(dirname "$0")/.."

STRICT=0
ONLY=""
for arg in "$@"; do
  case "$arg" in
    --strict) STRICT=1 ;;
    --only=*) ONLY="${arg#--only=}" ;;
    -h|--help)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

GO_IMAGE="golang:1.25"
CHECK_TIMEOUT="${CHECK_TIMEOUT:-900}"

# bounded CMD... runs with a wall-clock ceiling; macOS ships no timeout(1), so
# fall back to gtimeout, then perl's alarm, then unbounded but announced.
if command -v timeout >/dev/null 2>&1; then
  bounded() { timeout "$CHECK_TIMEOUT" "$@"; }
elif command -v gtimeout >/dev/null 2>&1; then
  bounded() { gtimeout "$CHECK_TIMEOUT" "$@"; }
elif command -v perl >/dev/null 2>&1; then
  bounded() { perl -e 'alarm shift; exec @ARGV or exit 127' "$CHECK_TIMEOUT" "$@"; }
else
  echo "note: no timeout(1), gtimeout or perl — checks run without a time limit" >&2
  bounded() { "$@"; }
fi
PASS=(); FAIL=(); SKIP=()

# run NAME CMD... is a check that must exit 0.
run() {
  local name="$1"; shift
  [ -z "$ONLY" ] || case "$name" in *"$ONLY"*) ;; *) return 0 ;; esac
  printf '── %-46s' "$name"
  local out rc
  # timeout(1) cannot exec a shell function, so functions (in_linux) are called
  # directly and carry their own bound.
  if declare -F "$1" >/dev/null 2>&1; then
    out=$("$@" 2>&1); rc=$?
  else
    out=$(bounded "$@" 2>&1); rc=$?
  fi
  # 124 from timeout(1)/gtimeout, 142 (128+SIGALRM) from the perl fallback.
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 142 ]; then
    echo "TIMEOUT"; FAIL+=("$name (timed out after ${CHECK_TIMEOUT}s)")
    return 0
  fi
  if [ "$rc" -eq 0 ]; then
    echo "PASS"; PASS+=("$name")
  else
    echo "FAIL"; FAIL+=("$name")
    # Named failures first: `go test ./...` ends with a per-package summary, so
    # the `--- FAIL` line is usually far above the tail.
    printf '%s\n' "$out" \
      | grep -E '^[[:space:]]*--- FAIL|^panic:|^fatal error:|^[[:space:]]*DATA RACE' \
      | head -20 | sed 's/^/    │ ! /'
    printf '%s\n' "$out" | sed 's/^/    │ /' | tail -25
  fi
}

# skippable NAME PREREQ_CMD CMD... reports SKIP when the prerequisite is absent.
skippable() {
  local name="$1" why="$2"; shift 2
  [ -z "$ONLY" ] || case "$name" in *"$ONLY"*) ;; *) return 0 ;; esac
  if ! eval "$why" >/dev/null 2>&1; then
    printf '── %-46s%s\n' "$name" "SKIP"
    SKIP+=("$name — prerequisite unavailable: $why")
    return 0
  fi
  run "$name" "$@"
}

# Containers are named and killed on exit: `--rm` removes a stopped container
# but an interrupted script leaves it running.
CONTAINERS=()
cleanup_containers() {
  local c
  for c in "${CONTAINERS[@]:-}"; do
    [ -n "$c" ] && docker kill "$c" >/dev/null 2>&1
  done
}
trap cleanup_containers EXIT INT TERM

# bash defers a signal until the foreground `docker run` returns, so the trap
# misses an interrupted container; kill leftovers from a previous run here.
sweep_stale() {
  local stale
  stale=$(docker ps -q --filter "name=^check-" 2>/dev/null) || return 0
  [ -n "$stale" ] || return 0
  echo "note: killing $(printf '%s\n' "$stale" | wc -l | tr -d ' ') container(s) left by an interrupted run" >&2
  printf '%s\n' "$stale" | xargs -r docker kill >/dev/null 2>&1 || true
}
command -v docker >/dev/null 2>&1 && sweep_stale

# Name the native platform explicitly: Docker reuses a cached image for a tag
# regardless of architecture, and an amd64 pull on arm64 runs under QEMU forever.
# The checks only need GOOS=linux, not amd64.
host_platform() {
  case "$(uname -m)" in
    arm64|aarch64) echo "linux/arm64" ;;
    x86_64|amd64)  echo "linux/amd64" ;;
    *)             echo "linux/$(uname -m)" ;;
  esac
}

# remote_linux CMD... runs the Linux half on CHECK_HOST, which has real KVM and
# a native filesystem; the local container fallback cannot exercise KVM.
remote_linux() {
  local tag="check-$$"
  # --delete so a locally deleted file cannot linger; .claude excluded because
  # gosec walks worktrees under it.
  rsync -az --delete --exclude '.git' --exclude '.claude' --exclude 'node_modules' \
    -e "ssh -o StrictHostKeyChecking=yes -o BatchMode=yes" \
    ./ "$CHECK_HOST:~/$tag/" >/dev/null 2>&1 || return 2
  # shellcheck disable=SC2029  # $* is expanded locally on purpose
  ssh -o StrictHostKeyChecking=yes -o BatchMode=yes "$CHECK_HOST" \
    "set -o pipefail; cd ~/$tag && sudo -n docker run --rm -v \"\$PWD\":/src -w /src \
       -e HOME=/tmp $GO_IMAGE bash -c $(printf '%q' "$*")"
  local rc=$?
  ssh -o StrictHostKeyChecking=yes -o BatchMode=yes "$CHECK_HOST" "rm -rf ~/$tag" >/dev/null 2>&1
  return $rc
}

in_linux() {
  if [ -n "${CHECK_HOST:-}" ]; then
    remote_linux "$@"
    return $?
  fi
  local name="check-$$-${#CONTAINERS[@]}"
  CONTAINERS+=("$name")
  bounded docker run --rm --name "$name" --platform "$(host_platform)" \
    -v "$PWD":/src -w /src -e HOME=/tmp \
    "$GO_IMAGE" bash -c "$*"
}

if [ -n "${CHECK_HOST:-}" ]; then
  echo "── check.sh: Linux checks on $CHECK_HOST (native amd64, real KVM)"
else
  echo "── check.sh: Linux checks in a LOCAL container — set CHECK_HOST to use the"
  echo "   reference box instead. Local runs cannot exercise KVM at all."
fi
echo

run "build (default)"            go build ./...
run "build sandboxd-fleet"       env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/sandboxd-fleet
run "build sandboxd-fleet -tags core" env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags core -o /dev/null ./cmd/sandboxd-fleet
run "build sandboxd -tags dev"   env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags dev -o /dev/null ./cmd/sandboxd

# -timeout: the race detector makes internal/httpapi alone take ~11 min on the
# reference node, against Go's 600s default — measured 665s with a wave's new
# tests and 654s without, so the default was already marginal and every agent
# read the resulting panic as a race. 30m is the per-package budget, not the
# suite's.
skippable "go test -race (linux)" "command -v docker" \
  in_linux "go test -race -count=1 -timeout 30m ./..."
run "no package silently skips its tests"  ./scripts/no-silent-skips.sh

run "tier boundary"              ./scripts/tier-boundary-guard.sh
run "actions pinned + scoped"    ./scripts/actions-pin-guard.sh
run "sandboxd units hardened"    ./scripts/unit-hardening-guard.sh

# The scan installs four tools and walks the whole tree, so it gets a longer ceiling.
CHECK_TIMEOUT="${SCAN_TIMEOUT:-2700}" \
skippable "security scan (linux)" "command -v docker" \
  in_linux "./scripts/security-scan.sh"
skippable "golangci-lint"        "command -v golangci-lint" \
  golangci-lint run --timeout 5m
skippable "gitleaks"             "command -v gitleaks" \
  gitleaks detect --source . --log-opts=--all --no-banner

# The exact commands ci.yml runs.
skippable "web typecheck"        "command -v pnpm && test -d web/node_modules" \
  bash -c 'cd web && pnpm exec tsc --noEmit'
skippable "web tests"            "command -v pnpm && test -d web/node_modules" \
  bash -c 'cd web && pnpm test'

echo
# A filter that selects nothing must not report OK.
if [ $(( ${#PASS[@]} + ${#FAIL[@]} + ${#SKIP[@]} )) -eq 0 ]; then
  echo "no checks matched --only=${ONLY} — nothing ran, which is not a pass." >&2
  exit 2
fi
echo "── ${#PASS[@]} passed, ${#FAIL[@]} failed, ${#SKIP[@]} skipped"
if [ "${#SKIP[@]}" -gt 0 ]; then
  echo
  echo "SKIPPED — these did not run, which is not the same as passing:"
  printf '  %s\n' "${SKIP[@]}"
  echo
  echo "  Install the missing prerequisite, or run with --strict to treat a skip"
  echo "  as a failure. CI runs everything; a green check.sh with skips does not"
  echo "  mean CI would be green."
fi
if [ "${#FAIL[@]}" -gt 0 ]; then
  echo
  echo "FAILED:"; printf '  %s\n' "${FAIL[@]}"
  exit 1
fi
if [ "$STRICT" = "1" ] && [ "${#SKIP[@]}" -gt 0 ]; then
  echo
  echo "--strict: refusing to report success with ${#SKIP[@]} check(s) unrun."
  exit 1
fi
echo
echo "OK"

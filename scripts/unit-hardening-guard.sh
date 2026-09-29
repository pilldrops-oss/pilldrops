#!/usr/bin/env bash
# unit-hardening-guard.sh: every file that writes a sandboxd systemd unit must
# carry the same hardening directives. MemoryMax here bounds the daemon only;
# guest memory is charged to the per-VMM cgroup.
#
# Usage: ./scripts/unit-hardening-guard.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# Values are not checked; a different MemoryMax is tuning, a missing one is the bug.
REQUIRED=(MemoryMax TasksMax NoNewPrivileges)

# Writers are found, not listed, so a new one cannot go unnoticed. This script
# excludes itself: its remedy text contains every directive it requires. A read
# loop rather than mapfile because macOS ships bash 3.2.
WRITERS=()
SELF="scripts/$(basename "$0")"
while IFS= read -r f; do
  [ "$f" = "$SELF" ] && continue
  WRITERS+=("$f")
# git ls-files, not a tree grep: .claude/worktrees holds checkouts of other branches.
done < <(git ls-files -z '*.sh' '*.service' 2>/dev/null | xargs -0 grep -l 'ExecStart=.*sandboxd-linux' 2>/dev/null | sort)

# Known-unhardened writers, each with a reason. Currently none.
EXCEPT=()

FAIL=0
CHECKED=0

is_excepted() {
  local f="$1" e
  for e in "${EXCEPT[@]:-}"; do [ "$f" = "$e" ] && return 0; done
  return 1
}

if [ "${#WRITERS[@]}" -eq 0 ]; then
  # A broken matcher must not look like a clean tree.
  echo "FAIL: found no file writing a sandboxd unit — the matcher is broken, not the tree" >&2
  exit 1
fi

for f in "${WRITERS[@]}"; do
  if is_excepted "$f"; then
    echo "SKIP $f (known-unhardened, see EXCEPT in this script)"
    continue
  fi
  CHECKED=$((CHECKED + 1))
  for d in "${REQUIRED[@]}"; do
    if ! grep -Eq "^[[:space:]]*${d}=" "$f"; then
      echo "FAIL $f: no ${d}= in the sandboxd unit it writes"
      FAIL=$((FAIL + 1))
    fi
  done
done

if [ "$CHECKED" -eq 0 ]; then
  echo "FAIL: every sandboxd unit writer is excepted — the guard checks nothing" >&2
  exit 1
fi

if [ "$FAIL" -gt 0 ]; then
  cat <<'EOF'

Add the missing directives to that unit's [Service] block. The set the other
writers use:

  MemoryHigh=40%
  MemoryMax=60%
  TasksMax=8192
  NoNewPrivileges=yes

Percentages rather than absolutes so the same unit is correct on a 4 GB VPS and
a 256 GB node. NoNewPrivileges does not stop the jailer's setuid() drop to the
per-sandbox uid — it only blocks privilege GAIN at execve.
EOF
  exit 1
fi

echo "OK: $CHECKED sandboxd unit writer(s) carry ${REQUIRED[*]}."

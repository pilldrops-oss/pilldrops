#!/usr/bin/env bash
# actions-pin-guard.sh: every third-party GitHub Action must be pinned to a full
# commit SHA (tags and branches are mutable pointers in someone else's repo), and
# every workflow must declare a top-level permissions block.
#
# Usage: ./scripts/actions-pin-guard.sh [dir]   (default .github/workflows)
set -uo pipefail

DIR="${1:-.github/workflows}"
[ -d "$DIR" ] || { echo "no such directory: $DIR" >&2; exit 2; }

FAIL=0
UNPINNED=0
NOPERMS=0
CHECKED=0

shopt -s nullglob
FILES=("$DIR"/*.yml "$DIR"/*.yaml)
[ "${#FILES[@]}" -gt 0 ] || { echo "no workflow files in $DIR" >&2; exit 2; }

for f in "${FILES[@]}"; do
  # Local actions (./action) move with the checked-out commit, so skip them.
  while IFS= read -r line; do
    ref=$(printf '%s' "$line" | sed -E 's/^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*//; s/[[:space:]]*$//')
    case "$ref" in
      ./*|'') continue ;;
    esac
    CHECKED=$((CHECKED+1))
    spec=${ref%%#*}
    spec=$(printf '%s' "$spec" | sed -E 's/[[:space:]]*$//')
    comment=""
    case "$ref" in *'#'*) comment=${ref#*#} ;; esac

    if ! printf '%s' "$spec" | grep -Eq '^[^@[:space:]]+@[0-9a-f]{40}$'; then
      echo "FAIL $f: not pinned to a 40-char commit SHA: $spec"
      FAIL=$((FAIL+1)); UNPINNED=$((UNPINNED+1))
      continue
    fi
    # The version comment is what tells a reviewer which version they are approving.
    if [ -z "$(printf '%s' "$comment" | tr -d '[:space:]')" ]; then
      echo "FAIL $f: pinned but has no version comment: $spec"
      FAIL=$((FAIL+1)); UNPINNED=$((UNPINNED+1))
    fi
  done < <(grep -E '^[[:space:]]*-?[[:space:]]*uses:' "$f" 2>/dev/null)

  # Top-level only: jobs without their own block inherit the repository default.
  if ! grep -Eq '^permissions:' "$f"; then
    echo "FAIL $f: no top-level 'permissions:' block — jobs inherit the repository default"
    FAIL=$((FAIL+1)); NOPERMS=$((NOPERMS+1))
  fi
done

if [ "$CHECKED" -eq 0 ]; then
  # A broken matcher would otherwise look exactly like success.
  echo "FAIL: no third-party 'uses:' found in $DIR — the matcher is broken, not the tree" >&2
  exit 1
fi

if [ "$FAIL" -gt 0 ]; then
  echo
  echo "$FAIL problem(s)."
  if [ "${UNPINNED:-0}" -gt 0 ]; then
    echo
    echo "For an unpinned action, pin the commit SHA the tag currently resolves to:"
    echo "  curl -fsSL https://api.github.com/repos/OWNER/REPO/commits/TAG | jq -r .sha"
    echo "  uses: OWNER/REPO@<sha>  # vX.Y.Z"
  fi
  if [ "${NOPERMS:-0}" -gt 0 ]; then
    echo
    echo "For a missing permissions block, declare least privilege at the top of the"
    echo "workflow — jobs otherwise inherit the repository default, which is usually"
    echo "write:"
    echo "  permissions:"
    echo "    contents: read"
  fi
  exit 1
fi

echo "OK: $CHECKED third-party action reference(s) pinned to a SHA with a version comment;"
echo "    ${#FILES[@]} workflow(s) declare an explicit permissions block."

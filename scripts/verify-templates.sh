#!/usr/bin/env bash
# verify-templates.sh: boot every catalog template and prove it contains the
# tooling its card advertises. Run before listing or relisting templates.
#
#   API_URL=https://pilldrops.com API_KEY=sf_... ./scripts/verify-templates.sh
#   ./scripts/verify-templates.sh --all          # include android family (slow)
#   ./scripts/verify-templates.sh security loadtest   # just these ids
#
# For each template: create, exec its probe, destroy. PASS = probe exit 0.
set -u

API_URL="${API_URL:-http://localhost:8080}"
API_KEY="${API_KEY:-}"
INCLUDE_HEAVY=0
ONLY=()
for a in "$@"; do
  case "$a" in
    --all) INCLUDE_HEAVY=1 ;;
    *) ONLY+=("$a") ;;
  esac
done

command -v jq >/dev/null || { echo "jq required" >&2; exit 2; }
auth=(-H "Authorization: Bearer ${API_KEY}")

# Keep in sync with the Preinstalled fields in internal/templates/catalog.go.
declare -A PROBES=(
  [base]='bash --version && git --version && curl --version'
  # A container that RAN, not an installed binary. Measured: with the data root
  # left on the guest's own overlayfs, `docker --version` + `compose version` +
  # `buildx version` + `test -S /var/run/docker.sock` all pass on a daemon where
  # every single `docker run` dies with EINVAL. Only the run catches that.
  [docker]='[ "$(docker run --rm alpine echo ok)" = ok ] && docker compose version && docker buildx version'
  [code-interpreter]='python3 -c "import numpy, pandas, matplotlib, scipy, sklearn" && node --version'
  [fastapi]='python3 -c "import fastapi, uvicorn, sqlalchemy, httpx"'
  [node]='node --version && tsx --version && tsc --version'
  [nextjs]='node -e "require(\"/root/app/package.json\")" && test -d /root/app/node_modules/next'
  [go]='go version'
  [rust]='rustc --version && cargo --version'
  [claude]='claude --version'
  [codex]='codex --version'
  [grok]='grok --version'
  [opencode]='opencode --version'
  [amp]='amp --version'
  [openclaw]='python3 -c "import playwright, selenium, bs4, flask" && command -v chromium'
  [playwright]='npx playwright --version && test -d "${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}"'
  [browser]='test -x /usr/local/bin/start.sh && ls /ms-playwright | grep -q chromium'
  [desktop]='command -v chromium && pgrep -x chromium >/dev/null'
  [security]='osv-scanner --version && gitleaks version && command -v pdscan && test -x /usr/local/bin/start.sh'
  [loadtest]='k6 version && command -v pdload && test -x /usr/local/bin/start.sh'
)
# KVM emulator templates take minutes to boot; only with --all.
declare -A HEAVY_PROBES=(
  [android]='adb version'
  [android-headless]='adb version'
  [mobile-test]='adb version && test -f /usr/local/bin/harness.sh || adb version'
)

ids=()
if [ ${#ONLY[@]} -gt 0 ]; then
  ids=("${ONLY[@]}")
else
  ids=("${!PROBES[@]}")
  [ "$INCLUDE_HEAVY" = 1 ] && ids+=("${!HEAVY_PROBES[@]}")
fi

# The screen stack every template image carries, which is what
# templates.HasDesktopStack declares and therefore what has_desktop promises.
# Derived from the one layer definition rather than re-listed here: a
# hand-kept third copy is how this list ended up missing packages the layer
# installs. The line after --no-install-recommends is the package list; three
# packages ship their command under another name.
# Prepended to every probe rather than kept as one `base` case: the claim is
# made for every template, so it has to be checked on every template.
# `command -v a b c` exits 0 when only ONE name resolves, in dash and in bash —
# hence the loop.
#
# xclip and xdpyinfo look like diagnostics and are load-bearing: without them
# the clipboard and resize checks cannot read a value, and a check that reads
# nothing reports the same empty string whether the feature works or not. A
# clipboard control passed exactly that way on 2026-09-20 — both the reading
# and its control were empty because xclip was absent. This probe is where a
# missing tool fails LOUDLY, so the behaviour checks downstream can trust that
# an empty reading means a broken feature and not a missing binary.
#
# PD_SKIP_DESKTOP_PROBE=1 drops the whole probe for a fleet built with
# PD_NO_DESKTOP=1.
DESKTOP_BINS=$(sed -n '/--no-install-recommends/{n;s/^ *//;s/[\\ ]*$//;p;}' \
  internal/runtime/docker/desktop-layer.dockerfile \
  | tr ' ' '\n' | sed -e 's/^xvfb$/Xvfb/' -e 's/^x11-utils$/xdpyinfo/' -e 's/^procps$/ps/')
[ -n "${DESKTOP_BINS}" ] || { echo "could not read the desktop layer's package list" >&2; exit 2; }
DESKTOP_PROBE="for c in ${DESKTOP_BINS}; do command -v \$c >/dev/null || { echo \"missing \$c\"; exit 1; }; done"

pass=0; fail=0; failed_ids=()
for id in "${ids[@]}"; do
  probe="${PROBES[$id]:-${HEAVY_PROBES[$id]:-}}"
  if [ -z "$probe" ]; then echo "?? $id (no probe defined)"; continue; fi
  if [ "${PD_SKIP_DESKTOP_PROBE:-0}" != 1 ]; then
    probe="${DESKTOP_PROBE} && ${probe}"
  fi

  resp=$(curl -sS "${auth[@]}" -H 'Content-Type: application/json' \
    -X POST "$API_URL/v1/sandboxes" \
    -d "{\"template\":\"$id\",\"timeout\":300}")
  sid=$(echo "$resp" | jq -r '.id // empty')
  if [ -z "$sid" ]; then
    echo "FAIL $id — create: $(echo "$resp" | jq -c . 2>/dev/null || echo "$resp")"
    fail=$((fail+1)); failed_ids+=("$id"); continue
  fi

  # Poll until the probe passes; the image may still be booting.
  ok=1
  for _ in $(seq 1 12); do
    out=$(curl -sS "${auth[@]}" -H 'Content-Type: application/json' \
      -X POST "$API_URL/v1/sandboxes/$sid/exec" \
      -d "$(jq -cn --arg c "$probe" '{cmd:("sh -lc " + ($c|@sh)), timeout:60}')")
    code=$(echo "$out" | jq -r '.exit_code // 1')
    [ "$code" = "0" ] && { ok=0; break; }
    sleep 5
  done

  if [ "$ok" = 0 ]; then
    echo "PASS $id"
    pass=$((pass+1))
  else
    echo "FAIL $id — probe: $(echo "$out" | jq -r '.stderr // .error // "no output"' | head -2 | tr '\n' ' ')"
    fail=$((fail+1)); failed_ids+=("$id")
  fi
  curl -sS "${auth[@]}" -X DELETE "$API_URL/v1/sandboxes/$sid" >/dev/null
done

echo
echo "verified: $pass passed, $fail failed${failed_ids:+ (${failed_ids[*]})}"
[ "$fail" = 0 ]

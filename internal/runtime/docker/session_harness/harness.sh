#!/bin/bash
# PillDrops mobile test session harness, injected by sandboxd into mobile-test
# sandboxes at boot.
#
# User contract (what `pilldrops test` automates):
#   1. Drop a script at /test/run.sh; it runs automatically.
#   2. Exit code 0 → GREEN, anything else → RED.
#   3. stdout/stderr stream live to the session page (run.log).
#   4. The emulator screen is recorded for the whole run (170s segments).
#   5. Files written to /test/artifacts/ are served in the final report.
#   6. Replacing /test/run.sh re-runs the suite (report resets).
#
# The session page and status feed are served on PD_PORT (default 8765), the
# sandbox preview URL. PD_TEST_DIR overrides the watched directory (default /test).
set -u
PORT="${PD_PORT:-8765}"
TD="${PD_TEST_DIR:-/test}"
D=/tmp/pd-session
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$D"
cp -f "$SELF_DIR/index.html" "$D/index.html"

NAMES=("Wait for Android boot" "Run test script (screen recording on)" "Collect artifacts: video · screenshot · log")
ST=(pend pend pend)
STATE=queued VERDICT=null RC=null
VIDS='[]' SHOTS='[]' ARTS='[]'

wr() {
  {
    printf '{"state":"%s","verdict":%s,"exit":%s,"device":"%s","steps":[' \
      "$STATE" "$VERDICT" "$RC" "${EMULATOR_DEVICE:-Android emulator} · Android 14"
    local i
    for i in "${!NAMES[@]}"; do
      [ "$i" -gt 0 ] && printf ','
      printf '{"n":"%s","s":"%s"}' "${NAMES[$i]}" "${ST[$i]}"
    done
    printf '],"videos":%s,"shots":%s,"artifacts":%s}' "$VIDS" "$SHOTS" "$ARTS"
  } > "$D/.status.tmp" && mv "$D/.status.tmp" "$D/status.json"
}

run_suite() {
  STATE=running VERDICT=null RC=null VIDS='[]' SHOTS='[]' ARTS='[]'
  ST=(run pend pend)
  : > "$D/run.log"
  wr

  local booted="" i
  for i in $(seq 1 100); do
    [ "$(adb shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ] && { booted=1; break; }
    sleep 3
  done
  if [ -n "$booted" ]; then ST[0]=pass; else ST[0]=fail; fi
  wr

  ST[1]=run; wr
  adb shell rm -f "/sdcard/pd-rec-*.mp4" >/dev/null 2>&1
  touch /tmp/pd-rec-on
  (
    n=1
    while [ -f /tmp/pd-rec-on ]; do
      adb shell "screenrecord --bit-rate 1200000 --time-limit 170 /sdcard/pd-rec-$n.mp4" >/dev/null 2>&1
      n=$((n + 1))
    done
  ) &
  local recloop=$!

  bash "$TD/run.sh" >> "$D/run.log" 2>&1
  RC=$?
  if [ "$RC" -eq 0 ]; then ST[1]=pass; else ST[1]=fail; fi
  wr

  ST[2]=run; wr
  rm -f /tmp/pd-rec-on
  adb shell pkill -2 screenrecord >/dev/null 2>&1
  sleep 4
  kill "$recloop" 2>/dev/null
  local f
  for f in $(adb shell ls /sdcard/pd-rec-*.mp4 2>/dev/null | tr -d '\r'); do
    adb pull "$f" "$D/" >/dev/null 2>&1
  done
  adb exec-out screencap -p > "$D/final.png" 2>/dev/null
  rm -rf "$D/artifacts"
  [ -d "$TD/artifacts" ] && cp -r "$TD/artifacts" "$D/artifacts" 2>/dev/null
  VIDS=$(python3 - "$D" <<'PY'
import json, os, sys
d = sys.argv[1]
print(json.dumps(sorted(f for f in os.listdir(d) if f.startswith("pd-rec-") and f.endswith(".mp4"))))
PY
)
  [ -s "$D/final.png" ] && SHOTS='["final.png"]' || SHOTS='[]'
  ARTS=$(python3 - "$D" <<'PY'
import json, os, sys
a = os.path.join(sys.argv[1], "artifacts")
out = []
if os.path.isdir(a):
    for root, _, files in os.walk(a):
        for f in files:
            out.append(os.path.relpath(os.path.join(root, f), a))
print(json.dumps(sorted(out)))
PY
)
  ST[2]=pass
  STATE=done
  if [ "$RC" -eq 0 ]; then VERDICT='"GREEN"'; else VERDICT='"RED"'; fi
  wr
}

wr
python3 -m http.server "$PORT" --bind 0.0.0.0 --directory "$D" >/dev/null 2>&1 &

# Re-run whenever /test/run.sh appears or changes.
last=""
while :; do
  if [ -f "$TD/run.sh" ]; then
    cur=$(md5sum "$TD/run.sh" 2>/dev/null | cut -d' ' -f1)
    if [ -n "$cur" ] && [ "$cur" != "$last" ]; then
      last="$cur"
      run_suite
    fi
  fi
  sleep 2
done

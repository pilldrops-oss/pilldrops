#!/usr/bin/env bash
# PillDrops GitHub Action core logic. Runs outside a GitHub runner too: set the
# INPUT_* variables, point GITHUB_OUTPUT and GITHUB_STEP_SUMMARY at files, execute.
#
# Inputs (env):
#   INPUT_API_KEY   required, PillDrops API key
#   INPUT_API_URL   required, your PillDrops server
#   INPUT_TEMPLATE  template id (default: base)
#   INPUT_RUN       command to execute; the job's pass/fail is this exit code
#   INPUT_SERVE     command to start a long-lived server instead of RUN
#   INPUT_PATH      directory/file to upload (default: .; "" or none to skip)
#   INPUT_DEST      where it lands in the sandbox (default: workspace). Directory
#                   uploads are always relative to the sandbox home, so a leading
#                   slash is stripped; single files honour absolute paths.
#   INPUT_PORT      app port to publish; required with SERVE, optional with RUN
#   INPUT_PUBLIC    "true" to make the preview URL tokenless
#   INPUT_TIMEOUT   sandbox lifetime in seconds (default: 900)
#   INPUT_KEEP      "true" to leave the sandbox running (implied by SERVE)
set -euo pipefail

die() { printf '::error::%s\n' "$*" >&2; exit 1; }
log() { printf '▸ %s\n' "$*"; }

API_KEY="${INPUT_API_KEY:-}"
API_URL="${INPUT_API_URL:-}"
[ -n "$API_URL" ] || die "api-url is required: the URL of your PillDrops server"
TEMPLATE="${INPUT_TEMPLATE:-base}"
CMD="${INPUT_RUN:-}"
SERVE="${INPUT_SERVE:-}"
SRC="${INPUT_PATH-.}"
DEST="${INPUT_DEST:-workspace}"
PORT="${INPUT_PORT:-}"
PUBLIC="${INPUT_PUBLIC:-false}"
TIMEOUT="${INPUT_TIMEOUT:-900}"
KEEP="${INPUT_KEEP:-false}"

[ -n "$API_KEY" ] || die "api-key is required (pass \${{ secrets.PILLDROPS_API_KEY }})"
[ -n "$CMD" ] || [ -n "$SERVE" ] || die "one of 'run' or 'serve' is required"
[ -z "$CMD" ] || [ -z "$SERVE" ] || die "use either 'run' or 'serve', not both"
[ -z "$SERVE" ] || [ -n "$PORT" ] || die "'serve' needs 'port' so the app has a preview URL"
[ "$SRC" = "none" ] && SRC=""

export PILLDROPS_API_KEY="$API_KEY"
[ -n "$API_URL" ] && export PILLDROPS_API_URL="$API_URL"
[ -n "$SERVE" ] && KEEP=true

command -v pilldrops >/dev/null 2>&1 || die "pilldrops CLI not found on PATH"

out() { [ -n "${GITHUB_OUTPUT:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"; return 0; }
summary() { [ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY"; return 0; }

create_args=(sandbox create --template "$TEMPLATE" --timeout "$TIMEOUT")
[ -n "$PORT" ] && create_args+=(--port "$PORT")
[ "$PUBLIC" = "true" ] && create_args+=(--public)

log "creating sandbox — template $TEMPLATE"
created=$(pilldrops --json "${create_args[@]}") || die "sandbox create failed"
SBID=$(printf '%s' "$created" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
[ -n "$SBID" ] || die "could not read sandbox id from create response: $created"
PREVIEW=$(printf '%s' "$created" | sed -n 's/.*"url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
log "sandbox $SBID ready"
out sandbox-id "$SBID"
[ -n "$PREVIEW" ] && out preview-url "$PREVIEW"

# Destroy on every exit path unless asked to keep it; a failed test must not leak a billed sandbox.
# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap below
cleanup() {
  if [ "$KEEP" = "true" ]; then
    log "keeping sandbox $SBID (keep/serve requested)"
    return 0
  fi
  log "destroying sandbox $SBID"
  pilldrops sandbox rm "$SBID" >/dev/null 2>&1 || printf '::warning::could not destroy sandbox %s\n' "$SBID"
}
trap cleanup EXIT

# WORKDIR is where the command runs, so `run: pytest` works on an uploaded repo.
# The CLI unpacks a directory relative to the sandbox home (leading slash dropped)
# but honours an absolute destination for a single file; mirror both.
WORKDIR=""
if [ -n "$SRC" ]; then
  [ -e "$SRC" ] || die "path not found: $SRC"
  if [ -d "$SRC" ]; then
    rel="${DEST#/}"
    log "uploading $SRC → \$HOME/$rel"
    pilldrops cp "$SRC" "$SBID:$rel" || die "upload failed"
    WORKDIR="\$HOME/$rel"
  else
    log "uploading $SRC → $DEST"
    pilldrops cp "$SRC" "$SBID:$DEST" || die "upload failed"
  fi
fi

if [ -n "$SERVE" ]; then
  log "starting server on port $PORT"
  # Set PORT explicitly: older sandbox agents export the template default (8000)
  # regardless of the published port.
  serve_cmd="PORT=$PORT $SERVE"
  [ -n "$WORKDIR" ] && serve_cmd="cd $WORKDIR && PORT=$PORT $SERVE"
  pilldrops start "$SBID" --port "$PORT" -- sh -c "$serve_cmd" || die "server failed to start"
  log "preview: ${PREVIEW:-<sandbox url>}"
  summary "### PillDrops preview"
  summary ""
  summary "Sandbox \`$SBID\` is serving on port $PORT."
  [ -n "$PREVIEW" ] && summary "" && summary "**[Open the preview →]($PREVIEW)**"
  out exit-code 0
  exit 0
fi

run_cmd="$CMD"
[ -n "$PORT" ] && run_cmd="PORT=$PORT $run_cmd"
[ -n "$WORKDIR" ] && run_cmd="cd $WORKDIR && ${PORT:+PORT=$PORT }$CMD"
log "running: $CMD"
echo "──────────────────────────────────────────"
set +e
pilldrops exec "$SBID" -- sh -c "$run_cmd"
RC=$?
set -e
echo "──────────────────────────────────────────"

out exit-code "$RC"
if [ "$RC" -eq 0 ]; then
  log "command passed"
  summary "### PillDrops run — ✅ passed"
else
  log "command failed with exit $RC"
  summary "### PillDrops run — ❌ failed (exit $RC)"
fi
summary ""
summary "Template \`$TEMPLATE\` · sandbox \`$SBID\`"
if [ -n "$PREVIEW" ]; then
  summary ""
  summary "Artifacts/report: [$PREVIEW]($PREVIEW)"
fi

exit "$RC"

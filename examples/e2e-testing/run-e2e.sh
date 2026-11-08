#!/usr/bin/env bash
# One-command E2E testing on PillDrops: spin up a pre-baked Playwright sandbox,
# run your tests against a target URL, and print a live report URL.
#
#   ./run-e2e.sh https://your-app.example.com  [./tests]
#
# Requires: `pilldrops login` done, and a tests/ dir of Playwright specs.
set -euo pipefail

TARGET="${1:?usage: ./run-e2e.sh <target-url> [tests-dir]}"
TESTS_DIR="${2:-./tests}"
CONFIG="${3:-./playwright.config.ts}"

echo "▸ creating pre-baked Playwright sandbox (browsers already installed)…"
ID=$(pilldrops --json sandbox create --template playwright --vcpu 2 --mem 4096 --timeout 3600 --idle -1 \
     | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
echo "  sandbox: $ID"
sleep 3

echo "▸ uploading tests + test runner (browsers are pre-baked — ~2s, no download)…"
pilldrops exec "$ID" -- bash -c 'mkdir -p /workspace/e2e/tests'
pilldrops cp "$CONFIG" "$ID:/e2e/playwright.config.ts"
for f in "$TESTS_DIR"/*; do pilldrops cp "$f" "$ID:/e2e/tests/$(basename "$f")"; done
pilldrops exec "$ID" --timeout 120 -- bash -c \
  'cd /workspace/e2e && npm init -y >/dev/null 2>&1 && npm install @playwright/test@1.49.0 --no-audit --no-fund >/dev/null 2>&1'

echo "▸ running tests (Chromium pre-baked — no browser download)…"
pilldrops exec "$ID" --timeout 300 -- bash -c \
  "cd /workspace/e2e && TARGET_URL='$TARGET' npx playwright test; echo EXIT:\$?" || true

echo "▸ serving the HTML report…"
pilldrops start "$ID" -- sh -c 'cd /workspace/e2e && npx playwright show-report playwright-report --host 0.0.0.0 --port $PORT'
sleep 4

echo
echo "════════════════════════════════════════════════════════════"
echo "  Report:  https://8000-$ID.pilldrops.app/"
echo "  (screenshots, screen recordings, traces, network logs)"
echo "════════════════════════════════════════════════════════════"

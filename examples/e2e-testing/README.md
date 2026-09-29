# E2E Testing on PillDrops — pre-baked Playwright sandbox

Run real headless-browser E2E tests against any URL and get a live report
(screenshots, screen recordings, traces, network logs) — all in an ephemeral
sandbox. Chromium and every browser dependency are **pre-baked**, so there's
nothing to install.

## One command

```bash
./run-e2e.sh https://your-app.example.com ./tests
```

It prints a report URL like `https://8000-<sandbox-id>.pilldrops.com/`.

## Or step by step

```bash
# 1. spin up the pre-baked browser sandbox (browsers already installed)
ID=$(pilldrops --json sandbox create --template playwright --idle -1 \
     | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# 2. upload your Playwright specs + config
pilldrops exec $ID -- mkdir -p /workspace/e2e/tests
pilldrops cp playwright.config.ts $ID:/e2e/playwright.config.ts
pilldrops cp tests/smoke.spec.ts  $ID:/e2e/tests/smoke.spec.ts

# 3. run the tests (no browser download — Chromium is baked in)
pilldrops exec $ID --timeout 300 -- \
  bash -c 'cd /workspace/e2e && TARGET_URL=https://your-app.example.com npx playwright test'

# 4. serve the HTML report (survives idle/resume)
pilldrops start $ID -- \
  sh -c 'cd /workspace/e2e && npx playwright show-report playwright-report --host 0.0.0.0 --port $PORT'
# → open  https://8000-$ID.pilldrops.com/
```

## What you get

- **HTML report** at a public URL — per-test pass/fail, timings
- **Screen recordings** (video) of every test
- **Traces** — step-by-step timeline with DOM snapshots
- **Screenshots** on every step/failure
- **Console + network logs** captured per test

## Why it's fast

- `--template playwright` boots the official Playwright image (Chromium/Firefox/
  WebKit + **all OS deps pre-baked**), so there is **no 2–4 min browser download**
  — that's the expensive part, and it's gone. The `@playwright/test` runner is a
  ~2s `npm install` (JS only, no browsers), which `run-e2e.sh` does for you.
- `--idle -1` keeps the report sandbox always-on so the URL stays live; drop it
  to let it idle-pause (it wakes on request).
- Bring your own `tests/` — anything `@playwright/test` supports works.

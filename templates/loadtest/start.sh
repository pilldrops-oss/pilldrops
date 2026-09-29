#!/bin/sh
# The preview port serves /root/loadtest — the report pdload writes (index.html,
# summary.json, run.log, script.js). Before the first run it shows a short
# how-to instead of an empty directory listing.
PORT="${PORT:-8000}"
mkdir -p /root/loadtest
if [ ! -f /root/loadtest/index.html ]; then
  cat > /root/loadtest/index.html <<'EOF'
<!doctype html><meta charset="utf-8"><title>Load test — PillDrops</title>
<style>body{margin:0;background:#131210;color:#f4f1e8;font:14px/1.7 ui-monospace,Menlo,monospace;display:flex;min-height:100vh;align-items:center;justify-content:center}
div{max-width:520px;padding:28px}code{color:#35c98a}h1{font-size:20px;letter-spacing:-.02em}p{color:#a8a294}</style>
<div><h1>No run yet</h1>
<p>This sandbox launched without a target URL, so there was nothing to load-test.</p>
<p><b>Easiest:</b> go back to the dashboard, open the <b>Load Test</b> card, type the
URL you want to test and hit <b>Run</b> — the GREEN/RED report will appear here.</p>
<p><b>Or from this sandbox's Terminal tab:</b></p>
<p><code>pdload https://your-api.example.com --vus 25 --duration 60s</code></p>
<p>The report replaces this page when the run finishes. Only test targets you own
or are authorised to test.</p></div>
EOF
fi
cd /root/loadtest
exec python3 -m http.server "$PORT" --bind 0.0.0.0

#!/bin/sh
# The preview port serves /root/security — the report pdscan writes. Before the
# first scan it shows how to start one rather than an empty directory listing.
PORT="${PORT:-8000}"
mkdir -p /root/security
if [ ! -f /root/security/index.html ]; then
  cat > /root/security/index.html <<'EOF'
<!doctype html><meta charset="utf-8"><title>Security scan — PillDrops</title>
<style>body{margin:0;background:#131210;color:#f4f1e8;font:14px/1.7 ui-monospace,Menlo,monospace;display:flex;min-height:100vh;align-items:center;justify-content:center}
div{max-width:560px;padding:28px}code{color:#35c98a}h1{font-size:20px;letter-spacing:-.02em}p{color:#a8a294}</style>
<div><h1>No scan yet</h1>
<p>This sandbox launched without anything to scan.</p>
<p><b>Easiest:</b> go back to the dashboard, open the <b>Security Review</b> card,
paste the URL (or upload the code) you want scanned and hit <b>Run</b> — the
GREEN/RED report will appear here.</p>
<p><b>Or from this sandbox's Terminal tab:</b> put code in <code>workspace/</code> and run</p>
<p><code>pdscan</code> &nbsp;·&nbsp; add <code>--url https://your-app.com</code> to also check response headers</p>
<p>Scan only code and targets you own.</p></div>
EOF
fi
cd /root/security
exec python3 -m http.server "$PORT" --bind 0.0.0.0

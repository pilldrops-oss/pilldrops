#!/bin/bash
# ─────────────────────────────────────────────────────────────────
# PillDrops API Demo: Deploy a Flask App to a Sandbox
# ─────────────────────────────────────────────────────────────────
# This script demonstrates the full PillDrops workflow:
#   1. Create a sandbox via API
#   2. Write project files (Flask app + templates)
#   3. Install dependencies
#   4. Run the app
#   5. Show the live URL
#
# Usage: ./examples/deploy_flask_app.sh
# Requires: devserver running on localhost:8080
# ─────────────────────────────────────────────────────────────────

set -euo pipefail
BASE="http://localhost:8080"
GREEN='\033[0;32m'
ORANGE='\033[0;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
NC='\033[0m'

echo ""
echo -e "${ORANGE}╔═══════════════════════════════════════════════════╗${NC}"
echo -e "${ORANGE}║  PillDrops API Demo: Deploy Flask App            ║${NC}"
echo -e "${ORANGE}╚═══════════════════════════════════════════════════╝${NC}"
echo ""

# ─── Step 1: Create Sandbox ─────────────────────────────────
echo -e "${CYAN}► Step 1: Creating sandbox...${NC}"
RESPONSE=$(curl -s -X POST "$BASE/v1/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{"template":"python-3.11","timeout":600}')
SB_ID=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
BOOT_MS=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin)['boot_ms'])")
echo -e "  ${GREEN}✓ Sandbox created: ${SB_ID} (${BOOT_MS}ms)${NC}"
echo ""

# ─── Step 2: Write Flask App ────────────────────────────────
echo -e "${CYAN}► Step 2: Writing Flask app files...${NC}"

# app.py
curl -s -X POST "$BASE/v1/sandboxes/$SB_ID/files?path=app.py" \
  -H 'Content-Type: application/octet-stream' \
  -d 'from flask import Flask, jsonify, render_template_string
import platform, sys, os, datetime

app = Flask(__name__)

HTML = """
<!DOCTYPE html>
<html>
<head>
  <title>Sandbox Flask App</title>
  <style>
    body { font-family: Inter, system-ui, sans-serif; background: #0a0a0a; color: #F5F2EE; margin: 0; padding: 40px; }
    .container { max-width: 800px; margin: 0 auto; }
    h1 { color: #FF8800; font-size: 2em; margin-bottom: 8px; }
    .subtitle { color: #77746F; margin-bottom: 32px; }
    .card { background: #141210; border: 1px solid #2D2925; border-radius: 12px; padding: 20px; margin-bottom: 16px; }
    .card h3 { color: #FF8800; margin: 0 0 12px 0; font-size: 14px; text-transform: uppercase; letter-spacing: 0.05em; }
    .row { display: flex; justify-content: space-between; padding: 6px 0; border-bottom: 1px solid #1E1A16; }
    .row:last-child { border-bottom: none; }
    .label { color: #77746F; }
    .value { color: #4ADE80; font-family: monospace; }
    .api-url { background: #1E1A16; padding: 8px 14px; border-radius: 6px; font-family: monospace; font-size: 13px; color: #60A5FA; margin-top: 8px; }
    .badge { display: inline-block; background: rgba(255,136,0,.15); color: #FF8800; padding: 2px 8px; border-radius: 4px; font-size: 12px; font-weight: 600; }
  </style>
</head>
<body>
  <div class="container">
    <h1>🔥 Flask App Running in Sandbox</h1>
    <p class="subtitle">Deployed via the PillDrops API • Sandbox ID: {{ sandbox_id }}</p>

    <div class="card">
      <h3>System Info</h3>
      <div class="row"><span class="label">Python</span><span class="value">{{ python_version }}</span></div>
      <div class="row"><span class="label">Platform</span><span class="value">{{ platform }}</span></div>
      <div class="row"><span class="label">Hostname</span><span class="value">{{ hostname }}</span></div>
      <div class="row"><span class="label">Time</span><span class="value">{{ time }}</span></div>
      <div class="row"><span class="label">PID</span><span class="value">{{ pid }}</span></div>
    </div>

    <div class="card">
      <h3>API Endpoints</h3>
      <div class="row"><span class="badge">GET</span><span class="value">/</span></div>
      <div class="row"><span class="badge">GET</span><span class="value">/api/status</span></div>
      <div class="row"><span class="badge">GET</span><span class="value">/api/env</span></div>
    </div>

    <div class="card">
      <h3>Try the API</h3>
      <div class="api-url">curl {{ request_url }}/api/status</div>
      <div class="api-url" style="margin-top:8px">curl {{ request_url }}/api/env</div>
    </div>
  </div>
</body>
</html>
"""

@app.route("/")
def index():
    return render_template_string(HTML,
        sandbox_id=os.environ.get("SANDBOX_ID", "unknown"),
        python_version=sys.version.split()[0],
        platform=platform.platform(),
        hostname=platform.node(),
        time=datetime.datetime.now().isoformat(),
        pid=os.getpid(),
        request_url="http://localhost:5000"
    )

@app.route("/api/status")
def status():
    return jsonify({
        "status": "running",
        "sandbox_id": os.environ.get("SANDBOX_ID", "unknown"),
        "python": sys.version,
        "uptime_seconds": 0,
    })

@app.route("/api/env")
def env():
    return jsonify({k: v for k, v in os.environ.items() if not k.startswith("_")})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000, debug=False)
'
echo -e "  ${GREEN}✓ app.py written${NC}"

# requirements.txt
curl -s -X POST "$BASE/v1/sandboxes/$SB_ID/files?path=requirements.txt" \
  -H 'Content-Type: application/octet-stream' \
  -d 'flask==3.1.1'
echo -e "  ${GREEN}✓ requirements.txt written${NC}"
echo ""

# ─── Step 3: Verify files ───────────────────────────────────
echo -e "${CYAN}► Step 3: Verifying files in workspace...${NC}"
RESULT=$(curl -s -X POST "$BASE/v1/sandboxes/$SB_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"ls -la"}')
echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','(empty)'))" | head -10
echo ""

# ─── Step 4: Install dependencies ───────────────────────────
echo -e "${CYAN}► Step 4: Installing Flask...${NC}"
RESULT=$(curl -s -X POST "$BASE/v1/sandboxes/$SB_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"pip install -q flask 2>&1 | tail -3","timeout":60}')
echo "$RESULT" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','') + d.get('stderr',''))" | tail -5
echo -e "  ${GREEN}✓ Dependencies installed${NC}"
echo ""

# ─── Step 5: Start the Flask app ────────────────────────────
echo -e "${CYAN}► Step 5: Starting Flask app on port 5000...${NC}"
curl -s -X POST "$BASE/v1/sandboxes/$SB_ID/exec" \
  -H 'Content-Type: application/json' \
  -d "{\"cmd\":\"SANDBOX_ID=$SB_ID python3 app.py &\",\"timeout\":3}" > /dev/null 2>&1 &
sleep 3
echo -e "  ${GREEN}✓ Flask app starting...${NC}"
echo ""

# ─── Step 6: Test the running app ───────────────────────────
echo -e "${CYAN}► Step 6: Testing the Flask app...${NC}"
echo ""

echo "  Testing GET /api/status:"
curl -s http://localhost:5000/api/status 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "  (Flask may still be starting...)"
echo ""

# ─── Step 7: Show sandbox info ──────────────────────────────
echo -e "${CYAN}► Step 7: Sandbox info via API...${NC}"
curl -s "$BASE/v1/sandboxes/$SB_ID" | python3 -m json.tool 2>/dev/null
echo ""

# ─── Summary ────────────────────────────────────────────────
echo -e "${ORANGE}═══════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  ✅ Flask app deployed to sandbox!${NC}"
echo ""
echo -e "  Sandbox ID:  ${CYAN}$SB_ID${NC}"
echo -e "  Workspace:   ${CYAN}$BASE/v1/sandboxes/$SB_ID${NC}"
echo -e "  Sandbox URL: ${CYAN}$BASE/sandbox/$SB_ID/${NC}"
echo -e "  Dashboard:   ${CYAN}$BASE/dashboard${NC}"
echo ""
echo -e "  ${ORANGE}API Endpoints:${NC}"
echo -e "    POST $BASE/v1/sandboxes/$SB_ID/exec"
echo -e "    POST $BASE/v1/sandboxes/$SB_ID/files?path=..."
echo -e "    GET  $BASE/v1/sandboxes/$SB_ID/files?path=..."
echo -e "    GET  $BASE/v1/sandboxes/$SB_ID"
echo -e "    DEL  $BASE/v1/sandboxes/$SB_ID"
echo ""
echo -e "  ${ORANGE}Try:${NC}"
echo -e "    curl $BASE/v1/sandboxes/$SB_ID/exec -d '{\"cmd\":\"cat app.py\"}'"
echo -e "    curl $BASE/v1/sandboxes/$SB_ID/files?path=app.py"
echo -e "${ORANGE}═══════════════════════════════════════════════════${NC}"

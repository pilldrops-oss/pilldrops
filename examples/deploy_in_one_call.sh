#!/bin/bash
# ─────────────────────────────────────────────────────────────
# Deploy a complete Todo App with ONE API call
# ─────────────────────────────────────────────────────────────
# Send files + setup + start in one request.
# The platform writes files, installs deps, starts the server.
# You get back the URL to the running app.
#
# Usage: ./examples/deploy_in_one_call.sh
# ─────────────────────────────────────────────────────────────

set -euo pipefail
BASE="${PILLDROPS_API_URL:-${E2B_API_URL:-http://localhost:8080}}"
GREEN='\033[0;32m'; ORANGE='\033[0;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

echo ""
echo -e "${BOLD}${ORANGE}Deploy Todo App — Single API Call${NC}"
echo ""

RESPONSE=$(curl -s -X POST "$BASE/v1/sandboxes" \
  -H 'Content-Type: application/json' \
  -d '{
  "template": "python-3.11",
  "timeout": 600,
  "port": 8000,
  "files": [
    {
      "path": "app.py",
      "content": "from flask import Flask, request, jsonify, send_from_directory\nimport uuid, os, json\n\napp = Flask(__name__, static_folder=\"static\")\nDB = \"todos.json\"\n\ndef load():\n    if os.path.exists(DB):\n        with open(DB) as f: return json.load(f)\n    return []\n\ndef save(t):\n    with open(DB, \"w\") as f: json.dump(t, f)\n\n@app.route(\"/\")\ndef index(): return send_from_directory(\"static\", \"index.html\")\n\n@app.route(\"/api/todos\", methods=[\"GET\"])\ndef list_todos(): return jsonify(load())\n\n@app.route(\"/api/todos\", methods=[\"POST\"])\ndef create():\n    d = request.json or {}; t = load()\n    item = {\"id\": str(uuid.uuid4())[:8], \"title\": d.get(\"title\", \"\"), \"done\": False}\n    t.append(item); save(t); return jsonify(item), 201\n\n@app.route(\"/api/todos/<tid>\", methods=[\"PATCH\"])\ndef update(tid):\n    d = request.json or {}; t = load()\n    for i in t:\n        if i[\"id\"] == tid:\n            if \"title\" in d: i[\"title\"] = d[\"title\"]\n            if \"done\" in d: i[\"done\"] = d[\"done\"]\n            save(t); return jsonify(i)\n    return jsonify({\"error\": \"not found\"}), 404\n\n@app.route(\"/api/todos/<tid>\", methods=[\"DELETE\"])\ndef delete(tid):\n    t = [i for i in load() if i[\"id\"] != tid]; save(t); return \"\", 204\n\nif __name__ == \"__main__\":\n    app.run(host=\"0.0.0.0\", port=8000)\n"
    },
    {
      "path": "static/index.html",
      "content": "<!DOCTYPE html><html><head><meta charset=\"UTF-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1.0\"><title>Todo App</title><style>*{box-sizing:border-box;margin:0;padding:0}body{font-family:Inter,-apple-system,system-ui,sans-serif;background:#0a0a0a;color:#f5f2ee;min-height:100vh;display:flex;justify-content:center;padding:40px 20px}.app{max-width:500px;width:100%}h1{font-size:28px;font-weight:700;color:#FF8800;margin-bottom:4px}.sub{color:#77746F;font-size:13px;margin-bottom:24px}.add{display:flex;gap:8px;margin-bottom:24px}.add input{flex:1;background:#1E1A16;border:1px solid #2D2925;border-radius:8px;padding:12px 16px;color:#f5f2ee;font-size:15px;outline:none}.add input:focus{border-color:#FF8800}.add button{background:#FF8800;color:#000;border:none;border-radius:8px;padding:12px 20px;font-weight:600;cursor:pointer}ul{list-style:none}.item{display:flex;align-items:center;gap:12px;padding:14px 16px;background:#141210;border:1px solid #2D2925;border-radius:10px;margin-bottom:8px}.item.done{opacity:.5}.item.done .t{text-decoration:line-through}.ck{width:22px;height:22px;border:2px solid #46423E;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0}.ck.on{background:#FF8800;border-color:#FF8800}.ck.on::after{content:\"✓\";color:#000;font-weight:700}.t{flex:1;font-size:15px}.x{color:#57544F;cursor:pointer;font-size:18px}.x:hover{color:#EF4444}.empty{text-align:center;color:#57544F;padding:40px}.cnt{color:#57544F;font-size:12px;text-align:center;margin-top:16px}.badge{display:inline-block;background:rgba(255,136,0,.15);color:#FF8800;font-size:11px;padding:2px 8px;border-radius:4px;font-weight:500;margin-left:8px}</style></head><body><div class=\"app\"><h1>📝 Todo App <span class=\"badge\">Sandbox</span></h1><p class=\"sub\">Deployed via single API call</p><div class=\"add\"><input id=\"i\" placeholder=\"What needs to be done?\" autofocus/><button onclick=\"add()\">Add</button></div><ul id=\"l\"></ul><div class=\"cnt\" id=\"c\"></div></div><script>var T=[];async function ld(){T=await(await fetch(\"/api/todos\")).json();rn()}function rn(){var l=document.getElementById(\"l\");if(!T.length){l.innerHTML=\"<li class=\\\"empty\\\">No todos yet. Add one above!</li>\";document.getElementById(\"c\").textContent=\"\";return}l.innerHTML=T.map(t=>\"<li class=\\\"item \"+(t.done?\"done\":\"\")+\"\\\"><div class=\\\"ck \"+(t.done?\"on\":\"\")+\"\\\" onclick=\\\"tg(\\'\"+ t.id+\"\\',\"+!t.done+\")\\\"></div><span class=\\\"t\\\">\"+esc(t.title)+\"</span><span class=\\\"x\\\" onclick=\\\"dl(\\'\"+ t.id+\"\\')\\\">&times;</span></li>\").join(\"\");var d=T.filter(t=>t.done).length;document.getElementById(\"c\").textContent=d+\" of \"+T.length+\" done\"}async function add(){var i=document.getElementById(\"i\"),v=i.value.trim();if(!v)return;i.value=\"\";await fetch(\"/api/todos\",{method:\"POST\",headers:{\"Content-Type\":\"application/json\"},body:JSON.stringify({title:v})});ld()}async function tg(id,d){await fetch(\"/api/todos/\"+id,{method:\"PATCH\",headers:{\"Content-Type\":\"application/json\"},body:JSON.stringify({done:d})});ld()}async function dl(id){await fetch(\"/api/todos/\"+id,{method:\"DELETE\"});ld()}function esc(s){var d=document.createElement(\"div\");d.textContent=s;return d.innerHTML}document.getElementById(\"i\").addEventListener(\"keydown\",e=>{if(e.key===\"Enter\")add()});ld();</script></body></html>"
    }
  ],
  "setup": [
    "pip3 install -q flask"
  ],
  "start": "python3 app.py",
  "port": 8000
}')

echo "$RESPONSE" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print()
print('  Sandbox:   \033[36m' + d['id'] + '\033[0m')
print('  Status:    ' + d['status'])
print('  Boot:      ' + str(d.get('boot_ms', '?')) + 'ms')
print()
if 'app_url' in d:
    print('  \033[1m\033[32m✅ App URL:  ' + d['app_url'] + '\033[0m')
    print()
if 'setup_log' in d:
    print('  Setup log:')
    for line in d['setup_log']:
        print('    ' + line)
print()
print('  Dashboard: \033[36m${BASE}/dashboard\033[0m')
print('  Sandbox:   \033[36m' + d.get('url', '') + '\033[0m')
print()
print('  \033[33mThe app is running! Open the App URL above.\033[0m')
print('  In the dashboard, select the sandbox and change port to :8000')
print()
"

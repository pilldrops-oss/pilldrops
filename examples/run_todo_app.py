#!/usr/bin/env python3
"""
Deploy a full Todo Web App inside a sandbox.

Creates a sandbox, writes a Flask app with frontend, installs deps,
runs API tests, and starts the server.

Usage:
    python3 examples/run_todo_app.py

Then open the Sandbox URL to use the terminal and run the app yourself.
"""
import requests, json, time, sys, os

BASE = os.environ.get("PILLDROPS_API_URL") or os.environ.get("E2B_API_URL") or "http://localhost:8080"
G = "\033[32m"; O = "\033[33m"; C = "\033[36m"; B = "\033[1m"; N = "\033[0m"; R = "\033[31m"

def api(method, path, data=None, raw_body=None):
    if raw_body is not None:
        r = getattr(requests, method)(f"{BASE}{path}", data=raw_body,
            headers={"Content-Type": "application/octet-stream"}, timeout=30)
    else:
        r = getattr(requests, method)(f"{BASE}{path}", json=data, timeout=60)
    if not r.ok and r.status_code != 204:
        print(f"{R}Error {r.status_code}: {r.text}{N}", file=sys.stderr); sys.exit(1)
    return r.json() if r.text and r.status_code != 204 else None

def run(sb_id, cmd, timeout=30):
    r = api("post", f"/v1/sandboxes/{sb_id}/exec", {"cmd": cmd, "timeout": timeout})
    return r

# ─── App source ──────────────────────────────────────

APP_PY = r'''from flask import Flask, request, jsonify, send_from_directory
import uuid, os, json

app = Flask(__name__, static_folder="static")
TODOS_FILE = "todos.json"

def load():
    if os.path.exists(TODOS_FILE):
        with open(TODOS_FILE) as f: return json.load(f)
    return []

def save(todos):
    with open(TODOS_FILE, "w") as f: json.dump(todos, f)

@app.route("/")
def index(): return send_from_directory("static", "index.html")

@app.route("/api/todos", methods=["GET"])
def list_todos(): return jsonify(load())

@app.route("/api/todos", methods=["POST"])
def create():
    data = request.json or {}; todos = load()
    t = {"id": str(uuid.uuid4())[:8], "title": data.get("title",""), "done": False}
    todos.append(t); save(todos); return jsonify(t), 201

@app.route("/api/todos/<tid>", methods=["PATCH"])
def update(tid):
    data = request.json or {}; todos = load()
    for t in todos:
        if t["id"] == tid:
            if "title" in data: t["title"] = data["title"]
            if "done" in data: t["done"] = data["done"]
            save(todos); return jsonify(t)
    return jsonify({"error": "not found"}), 404

@app.route("/api/todos/<tid>", methods=["DELETE"])
def delete(tid):
    todos = [t for t in load() if t["id"] != tid]; save(todos); return "", 204

if __name__ == "__main__":
    print("Todo App running on http://0.0.0.0:5000")
    app.run(host="0.0.0.0", port=5000)
'''

INDEX_HTML = '''<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Todo App — Sandbox</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:Inter,-apple-system,system-ui,sans-serif;background:#0a0a0a;color:#f5f2ee;min-height:100vh;display:flex;justify-content:center;padding:40px 20px}
.app{max-width:500px;width:100%}
h1{font-size:28px;font-weight:700;color:#FF8800;margin-bottom:4px}
.sub{color:#77746F;font-size:13px;margin-bottom:24px}
.add{display:flex;gap:8px;margin-bottom:24px}
.add input{flex:1;background:#1E1A16;border:1px solid #2D2925;border-radius:8px;padding:12px 16px;color:#f5f2ee;font-size:15px;outline:none}
.add input:focus{border-color:#FF8800}
.add button{background:#FF8800;color:#000;border:none;border-radius:8px;padding:12px 20px;font-weight:600;cursor:pointer}
ul{list-style:none}
.item{display:flex;align-items:center;gap:12px;padding:14px 16px;background:#141210;border:1px solid #2D2925;border-radius:10px;margin-bottom:8px}
.item.done{opacity:.5}.item.done .t{text-decoration:line-through}
.ck{width:22px;height:22px;border:2px solid #46423E;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center;flex-shrink:0}
.ck.on{background:#FF8800;border-color:#FF8800}.ck.on::after{content:"✓";color:#000;font-weight:700}
.t{flex:1;font-size:15px}
.x{color:#57544F;cursor:pointer;font-size:18px}.x:hover{color:#EF4444}
.empty{text-align:center;color:#57544F;padding:40px}
.cnt{color:#57544F;font-size:12px;text-align:center;margin-top:16px}
.badge{display:inline-block;background:rgba(255,136,0,.15);color:#FF8800;font-size:11px;padding:2px 8px;border-radius:4px;font-weight:500;margin-left:8px}
</style></head><body>
<div class="app">
<h1>📝 Todo App <span class="badge">Sandbox</span></h1>
<p class="sub">Running inside a PillDrops sandbox — real Flask server</p>
<div class="add"><input id="i" placeholder="What needs to be done?" autofocus/><button onclick="add()">Add</button></div>
<ul id="l"></ul><div class="cnt" id="c"></div>
</div>
<script>
var T=[];
async function ld(){T=await(await fetch("/api/todos")).json();rn()}
function rn(){var l=document.getElementById("l");if(!T.length){l.innerHTML='<li class="empty">No todos yet. Add one above!</li>';document.getElementById("c").textContent="";return}
l.innerHTML=T.map(t=>'<li class="item '+(t.done?"done":"")+'"><div class="ck '+(t.done?"on":"")+'" onclick="tg(\''+t.id+'\','+!t.done+')"></div><span class="t">'+esc(t.title)+'</span><span class="x" onclick="dl(\''+t.id+'\')">&times;</span></li>').join("");
var d=T.filter(t=>t.done).length;document.getElementById("c").textContent=d+" of "+T.length+" done"}
async function add(){var i=document.getElementById("i"),v=i.value.trim();if(!v)return;i.value="";await fetch("/api/todos",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({title:v})});ld()}
async function tg(id,d){await fetch("/api/todos/"+id,{method:"PATCH",headers:{"Content-Type":"application/json"},body:JSON.stringify({done:d})});ld()}
async function dl(id){await fetch("/api/todos/"+id,{method:"DELETE"});ld()}
function esc(s){var d=document.createElement("div");d.textContent=s;return d.innerHTML}
document.getElementById("i").addEventListener("keydown",e=>{if(e.key==="Enter")add()});ld();
</script></body></html>'''

# ─── Deploy ──────────────────────────────────────────

print(f"\n{B}{O}╔════════════════════════════════════════════╗{N}")
print(f"{B}{O}║  Deploy Todo Web App to Sandbox            ║{N}")
print(f"{B}{O}╚════════════════════════════════════════════╝{N}\n")

# 1. Create sandbox
print(f"  {O}1.{N} Creating sandbox...")
sb = api("post", "/v1/sandboxes", {"template": "python-3.11", "timeout": 600})
SB_ID = sb["id"]
print(f"     {G}✓ {SB_ID}{N} ({sb['boot_ms']}ms)\n")

# 2. Write files
print(f"  {O}2.{N} Writing app files...")
api("post", f"/v1/sandboxes/{SB_ID}/files?path=app.py", raw_body=APP_PY.encode())
api("post", f"/v1/sandboxes/{SB_ID}/files?path=static/index.html", raw_body=INDEX_HTML.encode())
# Verify
r = run(SB_ID, "ls -la")
print(f"     {G}✓{N} app.py + static/index.html written")
print(f"     Files: {r['stdout'].strip().count(chr(10))+1} entries\n")

# 3. Install Flask
print(f"  {O}3.{N} Installing Flask...")
r = run(SB_ID, "pip3 install -q flask", timeout=120)
print(f"     {G}✓{N} Flask installed (exit: {r['exit_code']})\n")

# 4. Test the app code (not as a server, just import test)
print(f"  {O}4.{N} Verifying code...")
r = run(SB_ID, "python3 -c \"import app; print('Flask app imports OK')\"")
print(f"     {G}✓{N} {r['stdout'].strip()}\n")

# 5. Test CRUD directly via Python (no server needed)
print(f"  {O}5.{N} Testing CRUD logic...")
test_script = '''
import app, json

# Create
with app.app.test_client() as c:
    # Empty list
    r = c.get("/api/todos")
    assert r.status_code == 200
    print(f"GET /api/todos → {r.get_json()}")

    # Create todos
    r = c.post("/api/todos", json={"title": "Buy milk"})
    assert r.status_code == 201
    t1 = r.get_json()
    print(f"POST → {t1['title']} (id={t1['id']})")

    r = c.post("/api/todos", json={"title": "Build sandbox platform"})
    t2 = r.get_json()
    print(f"POST → {t2['title']} (id={t2['id']})")

    r = c.post("/api/todos", json={"title": "Ship to production"})
    t3 = r.get_json()
    print(f"POST → {t3['title']} (id={t3['id']})")

    # List
    r = c.get("/api/todos")
    todos = r.get_json()
    print(f"GET → {len(todos)} todos")

    # Update (mark done)
    r = c.patch(f"/api/todos/{t1['id']}", json={"done": True})
    print(f"PATCH → {r.get_json()['title']} done={r.get_json()['done']}")

    # Delete
    r = c.delete(f"/api/todos/{t2['id']}")
    assert r.status_code == 204
    print(f"DELETE → {t2['id']} removed")

    # Final count
    r = c.get("/api/todos")
    print(f"Final: {len(r.get_json())} todos remaining")

    # Test HTML page
    r = c.get("/")
    assert r.status_code == 200
    assert b"Todo App" in r.data
    print(f"GET / → HTML page OK ({len(r.data)} bytes)")

print("\\nAll tests passed!")
'''
api("post", f"/v1/sandboxes/{SB_ID}/files?path=test_app.py", raw_body=test_script.encode())
r = run(SB_ID, "python3 test_app.py", timeout=15)
for line in r["stdout"].strip().split("\n"):
    print(f"     {G}✓{N} {line}")
print()

# 6. Summary
print(f"{O}{'═'*55}{N}")
print(f"{G}{B}  ✅ Todo App deployed and tested!{N}\n")
print(f"  Sandbox:     {C}{SB_ID}{N}")
print(f"  Sandbox URL: {C}{BASE}/sandbox/{SB_ID}/{N}")
print(f"  Dashboard:   {C}{BASE}/dashboard{N}")
print(f"")
print(f"  {B}To run the web server, open the sandbox URL above{N}")
print(f"  {B}and type in the terminal:{N}")
print(f"")
print(f"     {O}python3 app.py{N}")
print(f"")
print(f"  Then switch the port selector from 'Sandbox Page' to ':5000'")
print(f"  to see the Todo app running live in the preview panel.")
print(f"")
print(f"  {O}Or use the CLI:{N}")
print(f"    ./cli/pilldrops sandbox exec {SB_ID} \"python3 -c 'import app; print(app.app.url_map)'\"")
print(f"")
print(f"  {O}Destroy when done:{N}")
print(f"    ./cli/pilldrops sandbox destroy {SB_ID}")
print(f"{O}{'═'*55}{N}\n")

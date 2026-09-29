#!/usr/bin/env python3
"""
Deploy a full-stack Node.js Task Manager app to PillDrops.
Express API backend + Interactive HTML/CSS/JS frontend.

Usage: python3 examples/projects/fullstack_node.py
"""
import requests, json, sys, os, time

BASE = os.environ.get("PILLDROPS_API_URL") or os.environ.get("E2B_API_URL") or "http://localhost:8080"
G="\033[32m";O="\033[33m";C="\033[36m";B="\033[1m";N="\033[0m"

SERVER_JS = r'''const express = require("express");
const cors = require("cors");
const app = express();
const PORT = process.env.PORT || 8000;

app.use(cors());
app.use(express.json());
app.use(express.static("public"));

let tasks = [
  { id: 1, title: "Welcome to PillDrops!", status: "done", priority: "high", created: new Date().toISOString() },
  { id: 2, title: "Try creating a new task", status: "todo", priority: "medium", created: new Date().toISOString() },
  { id: 3, title: "Check the API at /api/tasks", status: "todo", priority: "low", created: new Date().toISOString() }
];
let nextId = 4;

app.get("/api/tasks", (req, res) => {
  let filtered = tasks;
  if (req.query.status) filtered = filtered.filter(t => t.status === req.query.status);
  res.json({ tasks: filtered, total: tasks.length });
});

app.post("/api/tasks", (req, res) => {
  const task = { id: nextId++, title: req.body.title || "Untitled", status: "todo", priority: req.body.priority || "medium", created: new Date().toISOString() };
  tasks.push(task);
  res.status(201).json(task);
});

app.patch("/api/tasks/:id", (req, res) => {
  const task = tasks.find(t => t.id === parseInt(req.params.id));
  if (!task) return res.status(404).json({ error: "not found" });
  Object.assign(task, req.body);
  res.json(task);
});

app.delete("/api/tasks/:id", (req, res) => {
  tasks = tasks.filter(t => t.id !== parseInt(req.params.id));
  res.status(204).end();
});

app.get("/api/stats", (req, res) => res.json({
  total: tasks.length,
  todo: tasks.filter(t => t.status === "todo").length,
  in_progress: tasks.filter(t => t.status === "in_progress").length,
  done: tasks.filter(t => t.status === "done").length,
}));

app.listen(PORT, "0.0.0.0", () => console.log("Task Manager on port " + PORT));
'''

FRONTEND_HTML = '''<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Task Manager</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui;background:#f8fafc;color:#1e293b;min-height:100vh}
.hd{background:linear-gradient(135deg,#FF8800,#FF5500);color:#fff;padding:20px 24px}
.hd h1{font-size:22px}.hd p{opacity:.85;font-size:13px;margin-top:2px}
.ct{max-width:700px;margin:0 auto;padding:20px}
.stats{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-bottom:20px}
.st{background:#fff;border-radius:10px;padding:16px;text-align:center;border:1px solid #e2e8f0}
.st .n{font-size:28px;font-weight:700;color:#FF8800}.st .l{font-size:11px;color:#94a3b8;text-transform:uppercase}
.ab{display:flex;gap:8px;margin-bottom:20px}
.ab input{flex:1;padding:12px;border:2px solid #e2e8f0;border-radius:10px;font-size:14px;outline:none}
.ab input:focus{border-color:#FF8800}
.ab select{padding:10px;border:2px solid #e2e8f0;border-radius:10px;background:#fff}
.ab button{background:#FF8800;color:#fff;border:none;border-radius:10px;padding:12px 20px;font-weight:600;cursor:pointer}
.fb{display:flex;gap:6px;margin-bottom:16px}
.fb button{padding:6px 14px;border-radius:20px;border:1px solid #e2e8f0;background:#fff;cursor:pointer;font-size:12px}
.fb button.on{background:#FF8800;color:#fff;border-color:#FF8800}
.tl{display:flex;flex-direction:column;gap:8px}
.tk{display:flex;align-items:center;gap:12px;background:#fff;border-radius:10px;padding:14px 16px;border:1px solid #e2e8f0}
.tk:hover{border-color:#FF8800}
.tk.dn{opacity:.5}.tk.dn .tt{text-decoration:line-through}
.ck{width:22px;height:22px;border:2px solid #cbd5e1;border-radius:6px;cursor:pointer;display:flex;align-items:center;justify-content:center}
.ck:hover{border-color:#FF8800}.ck.on{background:#FF8800;border-color:#FF8800}
.ck.on::after{content:"✓";color:#fff;font-weight:700}
.tt{flex:1;font-size:14px}
.bg{font-size:10px;padding:2px 8px;border-radius:10px;font-weight:600}
.bg-high{background:#fef2f2;color:#dc2626}.bg-medium{background:#fffbeb;color:#d97706}.bg-low{background:#f0fdf4;color:#16a34a}
.dl{color:#cbd5e1;cursor:pointer;font-size:18px}.dl:hover{color:#ef4444}
.em{text-align:center;padding:40px;color:#94a3b8}
.ft{text-align:center;padding:24px;color:#94a3b8;font-size:12px}.ft a{color:#FF8800}
</style>
</head>
<body>
<div class="hd"><h1>📋 Task Manager</h1><p>Full-stack Node.js app running in PillDrops</p></div>
<div class="ct">
<div class="stats" id="S"></div>
<div class="ab">
<input id="I" placeholder="Add a new task...">
<select id="P"><option value="high">🔴 High</option><option value="medium" selected>🟡 Medium</option><option value="low">🟢 Low</option></select>
<button onclick="add()">Add</button>
</div>
<div class="fb" id="F"></div>
<div class="tl" id="T"></div>
<div class="ft">Powered by <a href="/api/tasks">PillDrops API</a> — <a href="/api/stats">Stats</a></div>
</div>
<script>
let f='all';
async function ld(){
const[tr,sr]=await Promise.all([fetch('/api/tasks'+(f!=='all'?'?status='+f:'')),fetch('/api/stats')]);
const{tasks:t}=await tr.json(),s=await sr.json();
document.getElementById('S').innerHTML=['Total','To Do','In Progress','Done'].map((l,i)=>{
const v=[s.total,s.todo,s.in_progress,s.done][i];
return '<div class="st"><div class="n">'+v+'</div><div class="l">'+l+'</div></div>'}).join('');
document.getElementById('F').innerHTML=['all','todo','in_progress','done'].map(v=>
'<button class="'+(f===v?'on':'')+'" onclick="sf(\''+v+'\')">'+
(v==='all'?'All':v==='in_progress'?'In Progress':v[0].toUpperCase()+v.slice(1))+'</button>').join('');
if(!t.length){document.getElementById('T').innerHTML='<div class="em">No tasks</div>';return}
document.getElementById('T').innerHTML=t.map(k=>'<div class="tk'+(k.status==='done'?' dn':'')+'">'+
'<div class="ck'+(k.status==='done'?' on':'')+'" onclick="tg('+k.id+',\''+k.status+'\')"></div>'+
'<span class="tt">'+esc(k.title)+'</span>'+
'<span class="bg bg-'+k.priority+'">'+k.priority+'</span>'+
'<span class="dl" onclick="dl('+k.id+')">&times;</span></div>').join('')}
async function add(){const v=document.getElementById('I').value.trim();if(!v)return;
document.getElementById('I').value='';
await fetch('/api/tasks',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:v,priority:document.getElementById('P').value})});ld()}
async function tg(id,st){const nx=st==='todo'?'in_progress':st==='in_progress'?'done':'todo';
await fetch('/api/tasks/'+id,{method:'PATCH',headers:{'Content-Type':'application/json'},body:JSON.stringify({status:nx})});ld()}
async function dl(id){await fetch('/api/tasks/'+id,{method:'DELETE'});ld()}
function sf(v){f=v;ld()}
function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}
document.getElementById('I').onkeydown=e=>{if(e.key==='Enter')add()};
ld();
</script>
</body>
</html>'''

print(f"\n{B}{O}╔═══════════════════════════════════════════════════╗{N}")
print(f"{B}{O}║  Deploy Full-Stack Node.js App to PillDrops       ║{N}")
print(f"{B}{O}╚═══════════════════════════════════════════════════╝{N}\n")

# Deploy
print(f"  {O}►{N} Creating sandbox with files + deps + server...")
r = requests.post(f"{BASE}/v1/sandboxes", json={
    "template": "node-20",
    "timeout": 600,
    "port": 8000,
    "files": [
        {"path": "package.json", "content": json.dumps({"name":"task-manager","version":"1.0.0","dependencies":{"express":"^4.18.0","cors":"^2.8.5"}})},
        {"path": "server.js", "content": SERVER_JS},
        {"path": "public/index.html", "content": FRONTEND_HTML},
    ],
    "setup": ["npm install --production 2>&1 | tail -3"],
    "start": "node server.js",
}, timeout=120)

d = r.json()
sb_id = d["id"]
port = d.get("port", "?")
app_url = d.get("app_url", "")
boot = d.get("boot_ms", "?")

print(f"  {G}✓ Deployed in {boot}ms{N}")
if d.get("setup_log"):
    for line in d["setup_log"]:
        print(f"    {line}")
print()

# Test API
print(f"  {O}►{N} Testing backend API...")
time.sleep(1)

r = requests.get(f"http://localhost:{port}/api/tasks", timeout=5)
if r.ok:
    data = r.json()
    print(f"    {G}✓{N} GET /api/tasks → {data['total']} tasks")
else:
    print(f"    Waiting for server...")
    time.sleep(2)
    r = requests.get(f"http://localhost:{port}/api/tasks", timeout=5)
    data = r.json()
    print(f"    {G}✓{N} GET /api/tasks → {data['total']} tasks")

r = requests.post(f"http://localhost:{port}/api/tasks", json={"title":"Deploy to production","priority":"high"}, timeout=5)
print(f"    {G}✓{N} POST /api/tasks → created '{r.json()['title']}'")

r = requests.get(f"http://localhost:{port}/api/stats", timeout=5)
stats = r.json()
print(f"    {G}✓{N} GET /api/stats → total={stats['total']}, todo={stats['todo']}, done={stats['done']}")

# Test frontend
r = requests.get(f"http://localhost:{port}/", timeout=5)
has_title = "Task Manager" in r.text
print(f"    {G}✓{N} GET / → HTML frontend {'loaded' if has_title else 'error'} ({len(r.text)} bytes)")

# Test via proxy
r = requests.get(f"{app_url}", timeout=5)
print(f"    {G}✓{N} Preview proxy → {'OK' if r.ok else r.status_code}")

print(f"\n{O}{'═'*55}{N}")
print(f"{G}{B}  ✅ Full-Stack Node.js App Running!{N}\n")
print(f"  {B}Open in browser:{N}")
print(f"  {C}{app_url}{N}\n")
print(f"  {B}API endpoints:{N}")
print(f"    GET    /api/tasks         List all tasks")
print(f"    POST   /api/tasks         Create task")
print(f"    PATCH  /api/tasks/:id     Update status/priority")
print(f"    DELETE /api/tasks/:id     Delete task")
print(f"    GET    /api/stats         Dashboard stats\n")
print(f"  {B}Dashboard:{N}  {C}http://localhost:8080/dashboard{N}")
print(f"  {B}Sandbox:{N}    {C}{sb_id}{N}")
print(f"  {B}Port:{N}       {C}{port}{N}")
print(f"{O}{'═'*55}{N}\n")

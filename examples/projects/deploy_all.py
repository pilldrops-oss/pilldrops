#!/usr/bin/env python3
"""
Deploy 5 example projects to sandboxes — all via API.
Each project is a real running web app you can interact with.

Usage:
    python3 examples/projects/deploy_all.py

Requires: devserver running on localhost:8080 (make dev)
"""
import requests, json, sys, os, time

BASE = os.environ.get("PILLDROPS_API_URL") or os.environ.get("E2B_API_URL") or "http://localhost:8080"
G="\033[32m"; R="\033[31m"; O="\033[33m"; C="\033[36m"; B="\033[1m"; N="\033[0m"; M="\033[90m"

def deploy(name, payload):
    print(f"\n{O}━━━ {name} ━━━{N}")
    r = requests.post(f"{BASE}/v1/sandboxes", json=payload, timeout=120)
    if not r.ok:
        print(f"  {R}Failed: {r.status_code} {r.text[:100]}{N}")
        return None
    d = r.json()
    print(f"  {G}✓ Deployed{N} in {d.get('boot_ms','?')}ms")
    print(f"  ID:  {C}{d['id']}{N}")
    if d.get("app_url"):
        print(f"  App: {C}{d['app_url']}{N}")
    print(f"  URL: {C}{d.get('url','')}{N}")
    if d.get("setup_log"):
        for line in d["setup_log"]:
            print(f"  {M}  {line}{N}")
    return d


# ─── 1. Express REST API (Node.js) ──────────────────

express_api = deploy("1. Express REST API (Node.js)", {
    "template": "node-20", "timeout": 600, "port": 8000,
    "files": [
        {"path": "package.json", "content": json.dumps({
            "name": "todo-api", "version": "1.0.0",
            "dependencies": {"express": "^4.18.0", "cors": "^2.8.5", "uuid": "^9.0.0"}
        })},
        {"path": "server.js", "content": """const express = require('express');
const cors = require('cors');
const { v4: uuid } = require('uuid');

const app = express();
app.use(cors());
app.use(express.json());

let todos = [
  { id: uuid(), title: 'Learn sandbox platform', done: false, created: new Date() },
  { id: uuid(), title: 'Build something awesome', done: false, created: new Date() },
];

app.get('/', (req, res) => res.json({
  name: 'Todo API', version: '1.0.0', endpoints: ['GET /api/todos', 'POST /api/todos', 'PATCH /api/todos/:id', 'DELETE /api/todos/:id']
}));

app.get('/api/todos', (req, res) => res.json(todos));
app.post('/api/todos', (req, res) => {
  const todo = { id: uuid(), title: req.body.title || 'Untitled', done: false, created: new Date() };
  todos.push(todo);
  res.status(201).json(todo);
});
app.patch('/api/todos/:id', (req, res) => {
  const todo = todos.find(t => t.id === req.params.id);
  if (!todo) return res.status(404).json({ error: 'not found' });
  if (req.body.title !== undefined) todo.title = req.body.title;
  if (req.body.done !== undefined) todo.done = req.body.done;
  res.json(todo);
});
app.delete('/api/todos/:id', (req, res) => {
  todos = todos.filter(t => t.id !== req.params.id);
  res.status(204).end();
});

app.listen(8000, '0.0.0.0', () => console.log('Express Todo API on http://0.0.0.0:8000'));
"""},
    ],
    "setup": ["npm install --production 2>&1 | tail -3"],
    "start": "node server.js",
})


# ─── 2. React-style Frontend (static HTML) ──────────

frontend = deploy("2. Interactive Frontend (HTML/CSS/JS)", {
    "template": "python-3.11", "timeout": 600, "port": 8000,
    "files": [
        {"path": "index.html", "content": """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sandbox Dashboard</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui;background:linear-gradient(135deg,#0f0c29,#302b63,#24243e);color:#fff;min-height:100vh;padding:40px}
.container{max-width:800px;margin:0 auto}
h1{font-size:2.5em;background:linear-gradient(90deg,#FF8800,#FF4500);-webkit-background-clip:text;-webkit-text-fill-color:transparent;margin-bottom:8px}
.sub{color:#aaa;margin-bottom:32px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:16px;margin-bottom:32px}
.card{background:rgba(255,255,255,0.05);backdrop-filter:blur(10px);border:1px solid rgba(255,255,255,0.1);border-radius:16px;padding:24px;transition:transform 0.2s}
.card:hover{transform:translateY(-4px)}
.card h3{color:#FF8800;margin-bottom:8px;font-size:14px;text-transform:uppercase;letter-spacing:0.05em}
.card .value{font-size:2em;font-weight:700}
.card .unit{color:#888;font-size:14px}
.clock{font-family:monospace;font-size:3em;text-align:center;padding:32px;background:rgba(255,255,255,0.03);border-radius:16px;border:1px solid rgba(255,255,255,0.1);margin-bottom:32px}
.btn{display:inline-block;background:#FF8800;color:#000;padding:12px 24px;border:none;border-radius:8px;font-weight:600;cursor:pointer;font-size:14px;margin:4px}
.btn:hover{background:#FF6600}
.log{background:rgba(0,0,0,0.3);border-radius:8px;padding:16px;font-family:monospace;font-size:12px;max-height:200px;overflow-y:auto;color:#4ADE80}
</style>
</head>
<body>
<div class="container">
<h1>🚀 Sandbox Dashboard</h1>
<p class="sub">Running inside a PillDrops sandbox — fully interactive</p>
<div class="clock" id="clock">00:00:00</div>
<div class="grid">
<div class="card"><h3>Uptime</h3><div class="value" id="uptime">0</div><div class="unit">seconds</div></div>
<div class="card"><h3>Clicks</h3><div class="value" id="clicks">0</div><div class="unit">total</div></div>
<div class="card"><h3>Random</h3><div class="value" id="random">0</div><div class="unit">number</div></div>
<div class="card"><h3>Memory</h3><div class="value" id="memory">0</div><div class="unit">MB used</div></div>
</div>
<div style="text-align:center;margin-bottom:32px">
<button class="btn" onclick="clicks++;update()">Click Me!</button>
<button class="btn" onclick="addLog('Button clicked at '+new Date().toLocaleTimeString())">Log Event</button>
<button class="btn" onclick="document.body.style.background='linear-gradient(135deg,#'+Math.floor(Math.random()*16777215).toString(16)+',#'+Math.floor(Math.random()*16777215).toString(16)+',#'+Math.floor(Math.random()*16777215).toString(16)+')'">Random BG</button>
</div>
<div class="log" id="log"></div>
</div>
<script>
let clicks=0,start=Date.now();
function update(){
document.getElementById('uptime').textContent=Math.floor((Date.now()-start)/1000);
document.getElementById('clicks').textContent=clicks;
document.getElementById('random').textContent=Math.floor(Math.random()*1000);
document.getElementById('memory').textContent=(performance.memory?Math.floor(performance.memory.usedJSHeapSize/1024/1024):Math.floor(Math.random()*50+10));
var d=new Date();document.getElementById('clock').textContent=d.toLocaleTimeString();
}
function addLog(msg){var l=document.getElementById('log');l.innerHTML+='<div>['+new Date().toLocaleTimeString()+'] '+msg+'</div>';l.scrollTop=l.scrollHeight}
setInterval(update,1000);update();addLog('Dashboard loaded');
</script>
</body></html>"""},
    ],
    "setup": [],
    "start": "python3 -m http.server 8000",
})


# ─── 3. FastAPI + SQLite (Python) ────────────────────

fastapi_app = deploy("3. FastAPI + SQLite (Python API)", {
    "template": "fastapi", "timeout": 600, "port": 8000,
    "files": [
        {"path": "main.py", "content": """from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
import sqlite3, json

app = FastAPI(title="Notes API", version="1.0.0")

def get_db():
    conn = sqlite3.connect("notes.db")
    conn.execute("CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, title TEXT, content TEXT, created TEXT DEFAULT CURRENT_TIMESTAMP)")
    return conn

class NoteCreate(BaseModel):
    title: str
    content: str = ""

@app.get("/")
def root():
    return {"name": "Notes API", "docs": "/docs", "endpoints": ["/api/notes"]}

@app.get("/api/notes")
def list_notes():
    db = get_db()
    rows = db.execute("SELECT id, title, content, created FROM notes ORDER BY id DESC").fetchall()
    return [{"id": r[0], "title": r[1], "content": r[2], "created": r[3]} for r in rows]

@app.post("/api/notes", status_code=201)
def create_note(note: NoteCreate):
    db = get_db()
    cur = db.execute("INSERT INTO notes (title, content) VALUES (?, ?)", (note.title, note.content))
    db.commit()
    return {"id": cur.lastrowid, "title": note.title, "content": note.content}

@app.get("/api/notes/{note_id}")
def get_note(note_id: int):
    db = get_db()
    row = db.execute("SELECT id, title, content, created FROM notes WHERE id=?", (note_id,)).fetchone()
    if not row: raise HTTPException(404, "Note not found")
    return {"id": row[0], "title": row[1], "content": row[2], "created": row[3]}

@app.delete("/api/notes/{note_id}", status_code=204)
def delete_note(note_id: int):
    db = get_db()
    db.execute("DELETE FROM notes WHERE id=?", (note_id,))
    db.commit()

if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8000)
"""},
    ],
    "setup": ["pip3 install -q fastapi uvicorn pydantic"],
    "start": "python3 main.py",
})


# ─── 4. Chat App (Node.js + HTML) ───────────────────

chat_app = deploy("4. Real-time Chat App (Node.js)", {
    "template": "node-20", "timeout": 600, "port": 8000,
    "files": [
        {"path": "package.json", "content": json.dumps({
            "name": "chat-app", "version": "1.0.0",
            "dependencies": {"express": "^4.18.0"}
        })},
        {"path": "server.js", "content": """const express = require('express');
const app = express();
app.use(express.json());

let messages = [
  { id: 1, user: 'System', text: 'Welcome to the chat!', time: new Date().toISOString() }
];

app.get('/', (req, res) => res.send(`<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Chat</title><style>
*{box-sizing:border-box;margin:0;padding:0}body{font-family:system-ui;background:#1a1a2e;color:#eee;height:100vh;display:flex;flex-direction:column}
.header{background:#16213e;padding:16px 20px;font-weight:700;color:#FF8800;font-size:18px;border-bottom:1px solid #0f3460}
.messages{flex:1;overflow-y:auto;padding:16px;display:flex;flex-direction:column;gap:8px}
.msg{background:#16213e;border-radius:12px;padding:10px 14px;max-width:80%}.msg.self{align-self:flex-end;background:#0f3460}
.msg .user{color:#FF8800;font-size:12px;font-weight:600;margin-bottom:2px}.msg .text{font-size:14px}.msg .time{color:#666;font-size:10px;margin-top:4px}
.input-bar{display:flex;gap:8px;padding:12px;background:#16213e;border-top:1px solid #0f3460}
.input-bar input{flex:1;background:#1a1a2e;border:1px solid #0f3460;border-radius:8px;padding:10px;color:#eee;font-size:14px;outline:none}
.input-bar input:focus{border-color:#FF8800}
.input-bar button{background:#FF8800;color:#000;border:none;border-radius:8px;padding:10px 20px;font-weight:600;cursor:pointer}
</style></head><body>
<div class="header">💬 Sandbox Chat</div>
<div class="messages" id="msgs"></div>
<div class="input-bar"><input id="name" placeholder="Name" style="max-width:120px" value="User"><input id="msg" placeholder="Type a message..." autofocus><button onclick="send()">Send</button></div>
<script>
function render(msgs){var el=document.getElementById('msgs');el.innerHTML=msgs.map(m=>'<div class="msg"><div class="user">'+m.user+'</div><div class="text">'+m.text+'</div><div class="time">'+new Date(m.time).toLocaleTimeString()+'</div></div>').join('');el.scrollTop=el.scrollHeight}
function load(){fetch('/api/messages').then(r=>r.json()).then(render)}
function send(){var n=document.getElementById('name').value||'Anon',t=document.getElementById('msg').value;if(!t)return;document.getElementById('msg').value='';fetch('/api/messages',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:n,text:t})}).then(load)}
document.getElementById('msg').addEventListener('keydown',e=>{if(e.key==='Enter')send()});
setInterval(load,2000);load();
</script></body></html>`));

app.get('/api/messages', (req, res) => res.json(messages));
app.post('/api/messages', (req, res) => {
  const msg = { id: messages.length + 1, user: req.body.user || 'Anon', text: req.body.text, time: new Date().toISOString() };
  messages.push(msg);
  if (messages.length > 100) messages = messages.slice(-100);
  res.status(201).json(msg);
});

app.listen(8000, '0.0.0.0', () => console.log('Chat server on http://0.0.0.0:8000'));
"""},
    ],
    "setup": ["npm install --production 2>&1 | tail -3"],
    "start": "node server.js",
})


# ─── 5. Data Dashboard (Python + Charts) ────────────

data_dash = deploy("5. Data Dashboard (Python + Charts)", {
    "template": "python-3.11", "timeout": 600, "port": 8000,
    "files": [
        {"path": "app.py", "content": """from flask import Flask, jsonify, send_from_directory
import random, math, json

app = Flask(__name__, static_folder='.')

@app.route('/')
def index(): return send_from_directory('.', 'index.html')

@app.route('/api/data')
def data():
    # Generate realistic-looking sales data
    months = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec']
    sales = [random.randint(50,200) + i*10 for i in range(12)]
    users = [random.randint(100,500) + i*20 for i in range(12)]
    revenue = [s * random.uniform(25,45) for s in sales]
    return jsonify({
        'months': months, 'sales': sales, 'users': users,
        'revenue': [round(r,2) for r in revenue],
        'total_revenue': round(sum(revenue),2),
        'total_sales': sum(sales), 'total_users': sum(users),
        'growth': round((sales[-1]-sales[0])/sales[0]*100, 1)
    })

app.run(host='0.0.0.0', port=8000)
"""},
        {"path": "index.html", "content": """<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Data Dashboard</title><style>
*{box-sizing:border-box;margin:0;padding:0}body{font-family:system-ui;background:#0f172a;color:#e2e8f0;padding:24px}
h1{color:#FF8800;margin-bottom:4px;font-size:24px}.sub{color:#64748b;margin-bottom:24px;font-size:14px}
.metrics{display:grid;grid-template-columns:repeat(4,1fr);gap:16px;margin-bottom:24px}
.metric{background:#1e293b;border-radius:12px;padding:20px;border:1px solid #334155}
.metric .label{color:#94a3b8;font-size:12px;text-transform:uppercase;letter-spacing:0.05em;margin-bottom:4px}
.metric .val{font-size:28px;font-weight:700}.metric .val.green{color:#4ade80}.metric .val.blue{color:#60a5fa}.metric .val.orange{color:#FF8800}
.chart-container{background:#1e293b;border-radius:12px;padding:20px;border:1px solid #334155;margin-bottom:16px}
.chart-title{color:#94a3b8;font-size:12px;text-transform:uppercase;margin-bottom:12px}
.bar-chart{display:flex;align-items:flex-end;gap:8px;height:200px}
.bar{flex:1;background:linear-gradient(to top,#FF8800,#FF6600);border-radius:4px 4px 0 0;position:relative;transition:height 0.5s;min-width:0}
.bar:hover{opacity:0.8}.bar .tip{position:absolute;top:-20px;left:50%;transform:translateX(-50%);font-size:10px;color:#94a3b8;white-space:nowrap}
.bar .lbl{position:absolute;bottom:-18px;left:50%;transform:translateX(-50%);font-size:10px;color:#64748b}
button{background:#FF8800;color:#000;border:none;padding:10px 20px;border-radius:8px;font-weight:600;cursor:pointer;font-size:14px}
</style></head><body>
<h1>📊 Sales Dashboard</h1>
<p class="sub">Live data from sandbox API — <button onclick="load()">Refresh Data</button></p>
<div class="metrics" id="metrics"></div>
<div class="chart-container"><div class="chart-title">Monthly Sales</div><div class="bar-chart" id="chart"></div></div>
<div class="chart-container"><div class="chart-title">Monthly Revenue ($)</div><div class="bar-chart" id="chart2"></div></div>
<script>
function load(){fetch('/api/data').then(r=>r.json()).then(d=>{
document.getElementById('metrics').innerHTML=
'<div class="metric"><div class="label">Total Revenue</div><div class="val green">$'+d.total_revenue.toLocaleString()+'</div></div>'+
'<div class="metric"><div class="label">Total Sales</div><div class="val blue">'+d.total_sales.toLocaleString()+'</div></div>'+
'<div class="metric"><div class="label">Total Users</div><div class="val orange">'+d.total_users.toLocaleString()+'</div></div>'+
'<div class="metric"><div class="label">Growth</div><div class="val green">'+d.growth+'%</div></div>';
var max=Math.max(...d.sales);
document.getElementById('chart').innerHTML=d.sales.map((s,i)=>'<div class="bar" style="height:'+((s/max)*100)+'%"><div class="tip">'+s+'</div><div class="lbl">'+d.months[i]+'</div></div>').join('');
var maxR=Math.max(...d.revenue);
document.getElementById('chart2').innerHTML=d.revenue.map((r,i)=>'<div class="bar" style="height:'+((r/maxR)*100)+'%;background:linear-gradient(to top,#3b82f6,#60a5fa)"><div class="tip">$'+Math.round(r)+'</div><div class="lbl">'+d.months[i]+'</div></div>').join('');
})}load();
</script></body></html>"""},
    ],
    "setup": ["pip3 install -q flask"],
    "start": "python3 app.py",
})


# ─── Summary ─────────────────────────────────────────

print(f"\n\n{B}{O}{'═'*55}{N}")
print(f"{B}{G}  ✅ All 5 projects deployed!{N}\n")

projects = [
    ("Express REST API", express_api),
    ("Interactive Frontend", frontend),
    ("FastAPI + SQLite", fastapi_app),
    ("Chat App", chat_app),
    ("Data Dashboard", data_dash),
]

for name, p in projects:
    if p:
        url = p.get("app_url", p.get("url", ""))
        print(f"  {C}{name:30}{N} {url}")

print(f"\n  Dashboard: {C}{BASE}/dashboard{N}")
print(f"\n  Open any App URL above in your browser!")
print(f"  Or select a sandbox in the dashboard and set port to :8000")
print(f"{O}{'═'*55}{N}\n")

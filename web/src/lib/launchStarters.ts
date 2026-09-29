import type { Template } from '../types'
import { AGENT_CONFIG } from './launchAgents'

// START_FILE is where a plain sandbox's terminal start script is written.
export const START_FILE = '_pd_start.sh'

// universalTerminalScript serves a browser terminal (ttyd) on $PORT from any
// template. Tool availability differs per image, so it tries curl, wget and
// node for the download and falls back to a node or python HTTP page so $PORT
// is always bound.
export function universalTerminalScript(): string {
  return `#!/bin/sh
P="\${PORT:-8000}"
U="https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64"
have() { command -v "$1" >/dev/null 2>&1; }
if ! have curl && ! have wget && ! have node; then
  (have apt-get && apt-get update -qq && apt-get install -y -qq curl) >/dev/null 2>&1
  (have apk && apk add --no-cache curl) >/dev/null 2>&1
fi
if [ ! -x /usr/local/bin/ttyd ] && ! have ttyd; then
  if have curl; then curl -fsSL "$U" -o /usr/local/bin/ttyd 2>/dev/null
  elif have wget; then wget -qO /usr/local/bin/ttyd "$U" 2>/dev/null
  elif have node; then node -e 'const h=require("https"),f=require("fs");(function d(u){h.get(u,{headers:{"User-Agent":"pd"}},r=>{if(r.statusCode>299&&r.headers.location)return d(r.headers.location);const w=f.createWriteStream("/usr/local/bin/ttyd");r.pipe(w);w.on("finish",()=>process.exit(0))}).on("error",()=>process.exit(1))})("https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64")' 2>/dev/null
  fi
  chmod +x /usr/local/bin/ttyd 2>/dev/null
fi
T="$(command -v ttyd || echo /usr/local/bin/ttyd)"
[ -x "$T" ] && exec "$T" -W -p "$P" bash
if have node; then exec node -e 'require("http").createServer((q,s)=>{s.writeHead(200,{"Content-Type":"text/html"});s.end("<h2>Sandbox is running</h2><p>Open a terminal via exec, or use the CLI.</p>")}).listen(process.env.PORT||8000)'; fi
if have python3; then exec python3 -m http.server "$P"; fi
if have python; then exec python -m SimpleHTTPServer "$P"; fi
`
}

// Starter files bind $PORT so a fresh deploy always shows something.

const SALES_CSV = `month,region,units,revenue
Jan,North,120,14400
Jan,South,95,11400
Feb,North,132,15840
Feb,South,101,12120
Mar,North,150,18000
Mar,South,118,14160
Apr,North,141,16920
Apr,South,127,15240
May,North,163,19560
May,South,139,16680
Jun,North,171,20520
Jun,South,148,17760
`

const PY_ANALYSIS_STARTER = `# analysis.py — sample analysis: pandas + matplotlib over sales.csv,
# report served on $PORT. Edit it, redeploy, see your own analysis.
import os
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

df = pd.read_csv("sales.csv")
by_month = df.groupby("month", sort=False)["revenue"].sum()
by_region = df.groupby("region")["revenue"].sum()

fig, ax = plt.subplots(figsize=(7, 3.5))
by_month.plot(kind="bar", ax=ax, color="#0c8a56")
ax.set_title("Revenue by month")
ax.set_ylabel("USD")
fig.tight_layout()
fig.savefig("chart.png", dpi=120)

with open("index.html", "w") as f:
    f.write(
        "<!doctype html><meta charset='utf-8'><title>Sales analysis</title>"
        "<body style='font-family:system-ui;max-width:720px;margin:40px auto'>"
        "<h1>📊 Sales analysis</h1>"
        "<img src='chart.png' style='max-width:100%'>"
        "<h2>Revenue by region</h2>"
        + by_region.to_frame().to_html()
        + "<p>Edit <code>analysis.py</code> and redeploy to run your own.</p>"
    )

print(by_region)
print("report ready — serving on $PORT")
import http.server, socketserver
port = int(os.environ.get("PORT", "8000"))
with socketserver.TCPServer(("0.0.0.0", port), http.server.SimpleHTTPRequestHandler) as httpd:
    httpd.serve_forever()
`

const SCRAPER_STARTER = `# scrape.py — demo scraper: fetch a page, extract title + links, serve results.
# Describe YOUR target in the AI Configure box and this file gets rewritten.
import os, json
import requests
from bs4 import BeautifulSoup

URL = os.environ.get("SCRAPE_URL", "https://example.com")
soup = BeautifulSoup(requests.get(URL, timeout=20).text, "html.parser")
data = {
    "url": URL,
    "title": (soup.title.string or "").strip() if soup.title else "",
    "links": [a.get("href") for a in soup.find_all("a", href=True)][:50],
}
with open("index.html", "w") as f:
    f.write(
        "<!doctype html><meta charset='utf-8'><title>Scrape results</title>"
        "<body style='font-family:ui-monospace,monospace;max-width:720px;margin:40px auto'>"
        "<h1>🕷 " + data["title"] + "</h1><p>" + URL + "</p><pre>"
        + json.dumps(data, indent=2)[:20000] + "</pre>"
    )
print(json.dumps(data)[:500])
import http.server, socketserver
port = int(os.environ.get("PORT", "8000"))
with socketserver.TCPServer(("0.0.0.0", port), http.server.SimpleHTTPRequestHandler) as httpd:
    httpd.serve_forever()
`

const WEBHOOK_STARTER = `// inbox.js — webhook inbox: POST anything here, watch it appear live.
const http = require('http');
const PORT = process.env.PORT || 8000;
const hits = []; // newest first, capped
http.createServer((req, res) => {
  if (req.method === 'GET' && (req.url === '/' || req.url === '/index.html')) {
    res.writeHead(200, { 'Content-Type': 'text/html' });
    const rows = hits.map(h =>
      '<details><summary><b>' + h.method + '</b> ' + h.path + ' · ' + h.at +
      '</summary><pre>' + h.headers + '\\n\\n' + h.body.replace(/[<&]/g, c => c === '<' ? '&lt;' : '&amp;') +
      '</pre></details>').join('');
    res.end('<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="3">' +
      '<title>Webhook Inbox</title><body style="font-family:ui-monospace,monospace;max-width:760px;margin:40px auto">' +
      '<h1>📥 Webhook Inbox</h1><p>' + hits.length + ' request(s). Send anything to this URL — this page refreshes every 3s.</p>' +
      (rows || '<p><i>Waiting for the first request…</i></p>'));
    return;
  }
  let body = '';
  req.on('data', c => { body += c; if (body.length > 65536) req.destroy(); });
  req.on('end', () => {
    hits.unshift({ method: req.method, path: req.url, at: new Date().toISOString(),
      headers: JSON.stringify(req.headers, null, 2), body });
    if (hits.length > 200) hits.pop();
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{"ok":true}');
  });
}).listen(PORT, '0.0.0.0', () => console.log('inbox on ' + PORT));
`

const NODE_STARTER = `const http = require('http');
const PORT = process.env.PORT || 8000;

http.createServer((req, res) => {
  res.writeHead(200, { 'Content-Type': 'text/html' });
  res.end('<h1>🚀 Your Node sandbox is running</h1><p>Edit <code>server.js</code> and redeploy.</p>');
}).listen(PORT, '0.0.0.0', () => console.log('listening on ' + PORT));
`

const FASTAPI_STARTER = `from fastapi import FastAPI
from fastapi.responses import HTMLResponse

app = FastAPI()

@app.get("/", response_class=HTMLResponse)
def root():
    return "<h1>🚀 Your FastAPI sandbox is running</h1><p>Edit <code>main.py</code> and redeploy.</p>"
`

const GO_STARTER = `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<h1>🚀 Your Go sandbox is running</h1><p>Edit main.go and redeploy.</p>")
	})
	http.ListenAndServe("0.0.0.0:"+port, nil)
}
`

const GO_MOD = `module sandbox

go 1.22
`

const PY_HTTP_STARTER = `import http.server, socketserver, os

PORT = int(os.environ.get("PORT", "8000"))

class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(b"<h1>\\U0001F680 Your sandbox is running</h1><p>Edit app.py and redeploy.</p>")

socketserver.TCPServer(("0.0.0.0", PORT), Handler).serve_forever()
`


// starterFiles returns the default project files for a template, or [] when it
// ships its own server or needs a full uploaded project. The first file is the primary one.
export function starterFiles(tpl: Template): Array<{ path: string; content: string }> {
  if (tpl.has_server) return []
  // Coding-CLI agents generate their own code from a task.
  if (AGENT_CONFIG[tpl.id] || (tpl.tags ?? []).map(t => t.toLowerCase()).includes('coding-cli')) return []
  // Test templates run an uploaded suite, not a server.
  if ((tpl.tags ?? []).map(t => t.toLowerCase()).some(t => ['e2e', 'playwright', 'testing', 'test'].includes(t))) return []
  const id = (tpl.id || '').toLowerCase()
  const lang = (tpl.language || '').toLowerCase()
  const has = (...w: string[]) => w.some(x => id.includes(x) || lang === x)

  if (has('web-scraper', 'scraper'))
    return [{ path: 'scrape.py', content: SCRAPER_STARTER }]
  if (has('webhook-inbox', 'webhook-tester'))
    return [{ path: 'inbox.js', content: WEBHOOK_STARTER }]
  if (has('nextjs', 'next')) return []
  if (has('rust', 'cargo')) return []
  if (has('node', 'typescript', 'javascript') || lang === 'node' || lang === 'ts' || lang === 'js')
    return [{ path: 'server.js', content: NODE_STARTER }]
  if (has('fastapi')) return [{ path: 'main.py', content: FASTAPI_STARTER }]
  if (has('go', 'golang') || lang === 'go')
    return [
      { path: 'main.go', content: GO_STARTER },
      { path: 'go.mod', content: GO_MOD },
    ]
  if (has('openclaw')) return [{ path: 'app.py', content: PY_HTTP_STARTER }]
  if (has('code-interpreter', 'python', 'flask', 'django', 'jupyter') || lang === 'python')
    return [
      { path: 'analysis.py', content: PY_ANALYSIS_STARTER },
      { path: 'sales.csv', content: SALES_CSV },
    ]
  return []
}

// starterStart returns the start command for a template's starter when it
// differs from the generic suggestion; the python starter serves its own report.
export function starterStart(tpl: Template): string | undefined {
  const id = (tpl.id || '').toLowerCase()
  const lang = (tpl.language || '').toLowerCase()
  if (tpl.has_server) return undefined
  if (
    ['code-interpreter', 'python', 'jupyter'].some(x => id.includes(x)) ||
    lang === 'python'
  ) {
    if (id.includes('fastapi') || id.includes('openclaw')) return undefined
    return 'python3 analysis.py'
  }
  return undefined
}

// universalStartBody writes the terminal script to START_FILE and runs it.
export function universalStartBody(previewPort: number): {
  files: Array<{ path: string; content: string }>
  start: string
  port: number
} {
  return {
    files: [{ path: START_FILE, content: universalTerminalScript() }],
    start: `sh ${START_FILE}`,
    port: previewPort || 8000,
  }
}

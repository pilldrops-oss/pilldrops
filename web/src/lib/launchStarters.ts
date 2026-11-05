import type { Template } from '../types'

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

const PY_ANALYSIS_STARTER = `# analysis.py — sample analysis over sales.csv, report served on $PORT.
# Standard library only, so it runs on any Python image. Edit it, redeploy,
# see your own analysis.
import csv
import html
import http.server
import os
import socketserver
from collections import defaultdict

by_month, by_region = defaultdict(int), defaultdict(int)
with open("sales.csv") as f:
    for row in csv.DictReader(f):
        by_month[row["month"]] += int(row["revenue"])
        by_region[row["region"]] += int(row["revenue"])

top = max(by_month.values())
bars = "".join(
    f"<rect x='{i * 60 + 10}' y='{200 - v * 180 // top}' width='40' height='{v * 180 // top}' fill='#0c8a56'/>"
    f"<text x='{i * 60 + 30}' y='218' text-anchor='middle' font-size='12'>{html.escape(m)}</text>"
    for i, (m, v) in enumerate(by_month.items())
)
rows = "".join(f"<tr><td>{html.escape(r)}</td><td>{v:,}</td></tr>" for r, v in by_region.items())

with open("index.html", "w") as f:
    f.write(
        "<!doctype html><meta charset='utf-8'><title>Sales analysis</title>"
        "<body style='font-family:system-ui;max-width:720px;margin:40px auto'>"
        "<h1>📊 Sales analysis</h1><h2>Revenue by month</h2>"
        f"<svg width='{len(by_month) * 60 + 20}' height='225'>{bars}</svg>"
        f"<h2>Revenue by region</h2><table>{rows}</table>"
        "<p>Edit <code>analysis.py</code> and redeploy to run your own.</p>"
    )

print(dict(by_region))
print("report ready — serving on $PORT")
port = int(os.environ.get("PORT", "8000"))
with socketserver.TCPServer(("0.0.0.0", port), http.server.SimpleHTTPRequestHandler) as httpd:
    httpd.serve_forever()
`

const NODE_STARTER = `const http = require('http');
const PORT = process.env.PORT || 8000;

http.createServer((req, res) => {
  res.writeHead(200, { 'Content-Type': 'text/html' });
  res.end('<h1>🚀 Your Node sandbox is running</h1><p>Edit <code>server.js</code> and redeploy.</p>');
}).listen(PORT, '0.0.0.0', () => console.log('listening on ' + PORT));
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

// Standard library only: a crate with no dependencies builds without reaching crates.io.
const RUST_STARTER = `use std::io::{Read, Write};
use std::net::TcpListener;

fn main() {
    let port = std::env::var("PORT").unwrap_or_else(|_| "8000".into());
    let listener = TcpListener::bind(format!("0.0.0.0:{port}")).expect("bind $PORT");
    println!("listening on {port}");
    let body = "<h1>🚀 Your Rust sandbox is running</h1><p>Edit main.rs and redeploy.</p>";
    for mut stream in listener.incoming().flatten() {
        let mut request = [0u8; 1024];
        let _ = stream.read(&mut request);
        let _ = write!(
            stream,
            "HTTP/1.1 200 OK\\r\\nContent-Type: text/html; charset=utf-8\\r\\nContent-Length: {}\\r\\nConnection: close\\r\\n\\r\\n{}",
            body.len(),
            body
        );
    }
}
`

const CARGO_TOML = `[package]
name = "sandbox"
version = "0.1.0"
edition = "2021"

[[bin]]
name = "sandbox"
path = "main.rs"
`


// starterFiles returns the default project files for a template, or [] when it
// ships its own server or needs a full uploaded project. The first file is the primary one.
export function starterFiles(tpl: Template): Array<{ path: string; content: string }> {
  if (tpl.has_server) return []
  // Test templates run an uploaded suite, not a server.
  if ((tpl.tags ?? []).map(t => t.toLowerCase()).some(t => ['e2e', 'playwright', 'testing', 'test'].includes(t))) return []
  const id = (tpl.id || '').toLowerCase()
  const lang = (tpl.language || '').toLowerCase()
  const has = (...w: string[]) => w.some(x => id.includes(x) || lang === x)

  if (has('rust', 'cargo'))
    return [
      { path: 'main.rs', content: RUST_STARTER },
      { path: 'Cargo.toml', content: CARGO_TOML },
    ]
  if (has('node', 'typescript', 'javascript') || lang === 'node' || lang === 'ts' || lang === 'js')
    return [{ path: 'server.js', content: NODE_STARTER }]
  if (has('go', 'golang') || lang === 'go')
    return [
      { path: 'main.go', content: GO_STARTER },
      { path: 'go.mod', content: GO_MOD },
    ]
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
    return 'python3 analysis.py'
  }
  return undefined
}

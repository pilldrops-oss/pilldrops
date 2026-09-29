#!/usr/bin/env python3
"""Exercise real per-stack workloads (frontend builds, backend servers, AI SDK installs) across every template via the live sandbox API; never stops on failure.
Usage: PILLDROPS_API_URL=http://localhost:8080 PILLDROPS_API_KEY=sf_live_... python3 scripts/stack_coverage.py [--workers 3] [--only go,node]
"""
import argparse, json, os, sys, threading, time
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests

BASE = (os.environ.get("PILLDROPS_API_URL") or os.environ.get("E2B_API_URL") or "http://localhost:8080").rstrip("/")
KEY = os.environ.get("PILLDROPS_API_KEY") or os.environ.get("E2B_API_KEY", "")
G, R, O, C, B, N, M = "\033[32m", "\033[31m", "\033[33m", "\033[36m", "\033[1m", "\033[0m", "\033[90m"

RPS = float(os.environ.get("COVERAGE_RPS", "8"))
_gate = threading.Lock()
_next_at = [0.0]
_plock = threading.Lock()
_ctr = {"done": 0, "total": 0}


def _H(extra=None):
    h = {"Authorization": f"Bearer {KEY}"} if KEY else {}
    if extra: h.update(extra)
    return h


def _throttle():
    with _gate:
        now = time.monotonic()
        t = max(now, _next_at[0])
        _next_at[0] = t + 1.0 / RPS
        wait = t - now
    if wait > 0:
        time.sleep(wait)


def _http(method, url, **kw):
    r = None
    for attempt in range(7):
        _throttle()
        r = getattr(requests, method)(url, **kw)
        if r.status_code == 429:
            ra = r.headers.get("Retry-After")
            time.sleep(float(ra) if ra else min(2.0, 0.3 * (attempt + 1)))
            continue
        return r
    return r


def api(method, path, data=None, timeout=60):
    r = _http(method, f"{BASE}{path}", json=data, headers=_H(), timeout=timeout)
    if not r.ok:
        raise RuntimeError(f"{method.upper()} {path} -> {r.status_code} {r.text[:160]}")
    return None if (r.status_code == 204 or not r.text) else r.json()


def create(template="base", files=None, setup=None, start=None, port=0, timeout=600):
    body = {"template": template, "timeout": timeout}
    if files: body["files"] = files
    if setup: body["setup"] = setup
    if start: body["start"] = start
    if port: body["port"] = port
    return api("post", "/v1/sandboxes", body, timeout=180)


def execc(sid, cmd, timeout=120):
    return api("post", f"/v1/sandboxes/{sid}/exec", {"cmd": cmd, "timeout": timeout}, timeout=timeout + 30)


def destroy(sid):
    try:
        _http("delete", f"{BASE}/v1/sandboxes/{sid}", headers=_H(), timeout=30)
    except Exception:
        pass


def fs_write(sid, path, content):
    r = _http("post", f"{BASE}/v1/sandboxes/{sid}/files", params={"path": path},
              data=content.encode(), headers=_H({"Content-Type": "application/octet-stream"}), timeout=30)
    r.raise_for_status()


def curl_app(url, want=None, tries=80):
    last = None
    for _ in range(tries):
        try:
            r = _http("get", url, headers=_H(), timeout=5)
            last = r
            if r.status_code == 200 and (want is None or want in r.text):
                return r
        except Exception as e:
            last = e
        time.sleep(0.3)
    raise RuntimeError(f"app not ready: {getattr(last,'status_code',last)} body={getattr(last,'text','')[:120]!r}")


CASES = []

def C_exec(stack, name, template, script, expect="", files=None, timeout=120):
    def run():
        sid = create(template=template)["id"]
        try:
            for f in (files or []):
                fs_write(sid, f["path"], f["content"])
            r = execc(sid, script, timeout=timeout)
            out = r.get("stdout") or ""
            if r.get("exit_code", 0) != 0:
                raise RuntimeError(f"exit={r.get('exit_code')} {(r.get('stderr') or '')[-200:].strip()}")
            if expect and expect not in out:
                raise RuntimeError(f"want {expect!r} got {out.strip()[-160:]!r}")
            return (out.strip().splitlines()[-1][:80] if out.strip() else "ok")
        finally:
            destroy(sid)
    CASES.append({"stack": stack, "name": name, "run": run})

def C_serve(stack, name, template, files, setup, start, port, want, timeout=180):
    def run():
        resp = create(template=template, files=files, setup=setup, start=start, port=port)
        sid = resp.get("id")
        try:
            url = resp.get("app_url")
            if not url:
                raise RuntimeError(f"no app_url; deploy_log={resp.get('deploy_log')}")
            curl_app(url, want=want)
            return f"served {want!r} @ {url.split('/preview/')[-1]}"
        finally:
            destroy(sid)
    CASES.append({"stack": stack, "name": name, "run": run})


C_exec("versions", "node-version", "node", "node -v", "v")
C_exec("versions", "go-version", "go", "go version", "go1.")
C_exec("versions", "python-version", "python-3.11", "python3 --version", "Python 3")

C_serve("node-backend", "node-http", "node",
        [{"path": "server.js", "content":
          'const http=require("http");const p=process.env.PORT||8000;'
          'http.createServer((q,s)=>{s.writeHead(200);s.end("NODE-BACKEND-OK "+q.url)})'
          '.listen(p,"0.0.0.0");'}],
        [], "node server.js", 8000, "NODE-BACKEND-OK")

C_serve("node-backend", "express-api", "node",
        [{"path": "package.json", "content": '{"name":"api","version":"1.0.0","private":true}'},
         {"path": "app.js", "content":
          'const e=require("express")();'
          'e.get("/",(q,s)=>s.send("EXPRESS-OK"));'
          'e.listen(process.env.PORT||8000,"0.0.0.0");'}],
        ["npm install express --no-audit --no-fund --silent"], "node app.js", 8000, "EXPRESS-OK")

C_exec("node-backend", "npm-lodash", "node",
       'npm init -y >/dev/null 2>&1 && npm install lodash --no-audit --no-fund --silent && '
       'node -e \'console.log("LODASH-OK", require("lodash").chunk([1,2,3,4],2).length)\'',
       "LODASH-OK", timeout=120)

C_serve("frontend", "react-esbuild", "node",
        [{"path": "package.json", "content": '{"name":"fe","version":"1.0.0","private":true}'},
         {"path": "src/app.jsx", "content":
          'import React from "react";\n'
          'export const marker="FRONTEND-REACT-OK";\n'
          'export function App(){return React.createElement("div",null,marker);}\n'
          'console.log(marker);\n'},
         {"path": "serve.js", "content":
          'const http=require("http"),fs=require("fs");const p=process.env.PORT||8000;'
          'http.createServer((q,s)=>{s.writeHead(200,{"Content-Type":"application/javascript"});'
          's.end(fs.readFileSync("dist/bundle.js"))}).listen(p,"0.0.0.0");'}],
        ["npm install react react-dom esbuild --no-audit --no-fund --silent",
         "./node_modules/.bin/esbuild src/app.jsx --bundle --outfile=dist/bundle.js"],
        "node serve.js", 8000, "FRONTEND-REACT-OK", timeout=180)

C_serve("go-backend", "go-http", "go",
        [{"path": "main.go", "content":
          'package main\nimport("net/http";"os";"fmt")\n'
          'func main(){p:=os.Getenv("PORT");if p==""{p="8000"};'
          'http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"GO-BACKEND-OK ",r.URL.Path)});'
          'http.ListenAndServe("0.0.0.0:"+p,nil)}\n'}],
        ["go mod init app"], "go run main.go", 8000, "GO-BACKEND-OK", timeout=180)

C_exec("go-backend", "go-build", "go",
       "go mod init app && go build -o app main.go && ./app",
       "GO-BUILD-OK",
       files=[{"path": "main.go", "content":
               'package main\nimport "fmt"\nfunc main(){fmt.Println("GO-BUILD-OK")}\n'}],
       timeout=180)

C_exec("go-backend", "go-mod-get-uuid", "go",
       "go mod init app && go get github.com/google/uuid >/dev/null 2>&1 && go run m.go",
       "UUID-OK",
       files=[{"path": "m.go", "content":
               'package main\nimport("fmt";"github.com/google/uuid")\n'
               'func main(){fmt.Println("UUID-OK",len(uuid.New().String()))}\n'}],
       timeout=180)

C_exec("python-ai", "pip-anthropic", "python-3.11",
       'python3 -m venv .v && ./.v/bin/pip -q install anthropic && '
       './.v/bin/python -c \'import anthropic;anthropic.Anthropic(api_key="x");print("ANTHROPIC-PY-OK",anthropic.__version__)\'',
       "ANTHROPIC-PY-OK", timeout=300)

C_exec("python-ai", "pip-openai", "python-3.11",
       'python3 -m venv .v && ./.v/bin/pip -q install openai && '
       './.v/bin/python -c \'import openai;openai.OpenAI(api_key="x");print("OPENAI-PY-OK",openai.__version__)\'',
       "OPENAI-PY-OK", timeout=300)

C_exec("python-ai", "pip-langchain-core", "python-3.11",
       'python3 -m venv .v && ./.v/bin/pip -q install langchain-core && '
       './.v/bin/python -c \'import langchain_core;print("LANGCHAIN-OK",langchain_core.__version__)\'',
       "LANGCHAIN-OK", timeout=300)

C_exec("node-ai", "npm-anthropic-sdk", "claude",
       'npm init -y >/dev/null 2>&1 && npm install @anthropic-ai/sdk --no-audit --no-fund --silent && '
       'node --input-type=module -e \'import A from "@anthropic-ai/sdk"; import fs from "node:fs"; '
       'new A({apiKey:"x"}); console.log("ANTHROPIC-SDK-OK", '
       'JSON.parse(fs.readFileSync("node_modules/@anthropic-ai/sdk/package.json","utf8")).version)\'',
       "ANTHROPIC-SDK-OK", timeout=240)

C_exec("node-ai", "npm-openai-sdk", "codex",
       'npm init -y >/dev/null 2>&1 && npm install openai --no-audit --no-fund --silent && '
       'node --input-type=module -e \'import O from "openai"; import fs from "node:fs"; '
       'new O({apiKey:"x"}); console.log("OPENAI-SDK-OK", '
       'JSON.parse(fs.readFileSync("node_modules/openai/package.json","utf8")).version)\'',
       "OPENAI-SDK-OK", timeout=240)

C_exec("node-ai", "npx-run", "node",
       'npx --yes cowsay "AGENT-OK" 2>/dev/null | grep -q AGENT-OK && echo NPX-OK',
       "NPX-OK", timeout=180)

C_serve("python-backend", "fastapi", "python-3.11",
        [{"path": "app.py", "content":
          'from fastapi import FastAPI\napp=FastAPI()\n'
          '@app.get("/")\ndef root(): return {"msg":"FASTAPI-OK"}\n'}],
        ["python3 -m venv .v && ./.v/bin/pip -q install fastapi uvicorn"],
        './.v/bin/uvicorn app:app --host 0.0.0.0 --port ${PORT:-8000}', 8000, "FASTAPI-OK", timeout=180)

C_exec("data", "pandas-numpy", "code-interpreter",
       'python3 -m venv .v && ./.v/bin/pip -q install pandas numpy && '
       './.v/bin/python -c \'import pandas as pd,numpy as np;'
       'print("PANDAS-OK",int(pd.DataFrame({"x":np.arange(1,11)}).x.sum()))\'',
       "PANDAS-OK 55", timeout=300)

# No template image ships Deno, so these fail under real isolation; opt in with COVERAGE_INCLUDE_DENO=1.
if os.environ.get("COVERAGE_INCLUDE_DENO") == "1":
    C_exec("deno", "deno-version", "base", "deno --version | head -1", "deno", timeout=60)
    C_exec("deno", "deno-eval", "base", 'deno eval \'console.log("DENO-OK",1+2)\'', "DENO-OK 3", timeout=90)
    C_serve("deno", "deno-http", "base",
            [{"path": "main.ts", "content":
              'Deno.serve({port:Number(Deno.env.get("PORT")||8000),hostname:"0.0.0.0"},'
              '()=>new Response("DENO-HTTP-OK"));'}],
            [], "deno run --allow-net --allow-env main.ts", 8000, "DENO-HTTP-OK", timeout=120)

C_exec("shell", "base-uname", "base", "uname -s && echo BASE-OK", "BASE-OK")
C_exec("shell", "desktop-shell", "desktop", "echo DESKTOP-OK && ls / | head -1", "DESKTOP-OK")


def run_case(c):
    t0 = time.time()
    try:
        detail = c["run"]()
        ok, msg = True, detail or "ok"
    except Exception as e:
        ok, msg = False, str(e)[:220]
    ms = int((time.time() - t0) * 1000)
    with _plock:
        _ctr["done"] += 1
        n = _ctr["done"]
        tag = f"{G}✓{N}" if ok else f"{R}✗{N}"
        col = C if ok else R
        print(f"  {tag} [{n:>2}/{_ctr['total']}] {col}{c['stack']:<14}{N} {c['name']:<20} "
              f"{M}{ms/1000:6.1f}s{N}  {msg[:90]}")
    return {"stack": c["stack"], "name": c["name"], "ok": ok, "ms": ms, "msg": msg}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--workers", type=int, default=3)
    ap.add_argument("--only", default="", help="comma-separated stacks to include")
    args = ap.parse_args()
    if not KEY:
        print(f"{R}PILLDROPS_API_KEY not set{N}", file=sys.stderr); sys.exit(2)

    cases = CASES
    if args.only:
        want = set(s.strip() for s in args.only.split(","))
        cases = [c for c in CASES if c["stack"] in want]

    _ctr["total"] = len(cases)
    print(f"\n{B}stack coverage{N} → {BASE}   ({len(cases)} real workloads, {args.workers} workers)")
    print(f"{M}frontend builds · backend servers · AI-agent SDK installs · installs are sandbox-local{N}\n")

    results = []
    with ThreadPoolExecutor(max_workers=args.workers) as ex:
        futs = [ex.submit(run_case, c) for c in cases]
        for f in as_completed(futs):
            results.append(f.result())

    by = {}
    for r in results:
        d = by.setdefault(r["stack"], {"ok": 0, "n": 0})
        d["n"] += 1; d["ok"] += 1 if r["ok"] else 0
    print(f"\n{B}── stack summary ──{N}")
    for st in sorted(by):
        d = by[st]
        col = G if d["ok"] == d["n"] else (O if d["ok"] else R)
        print(f"  {col}{st:<15}{N} {d['ok']:>2}/{d['n']:<2}")
    fails = [r for r in results if not r["ok"]]
    if fails:
        print(f"\n  {R}failures:{N}")
        for r in fails:
            print(f"    {R}✗{N} {r['stack']}/{r['name']}: {r['msg'][:130]}")
    tot = sum(1 for r in results if r["ok"])
    col = G if tot == len(results) else O
    print(f"\n  {B}TOTAL: {col}{tot}/{len(results)} real workloads passed{N}\n")


if __name__ == "__main__":
    main()

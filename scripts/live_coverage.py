#!/usr/bin/env python3
"""Exercise ~100 use cases against a running sandboxd and report per-category coverage; never stops on failure.
Usage: PILLDROPS_API_URL=http://localhost:8080 PILLDROPS_API_KEY=sf_live_... python3 scripts/live_coverage.py [--keep-apps] [--loop N] [--workers N]
"""
import argparse, base64, codecs, hashlib, json, math, os, re, sys, threading, time, uuid
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests

BASE = (os.environ.get("PILLDROPS_API_URL") or os.environ.get("E2B_API_URL") or "http://localhost:8080").rstrip("/")
KEY = os.environ.get("PILLDROPS_API_KEY") or os.environ.get("E2B_API_KEY", "")
G, R, O, C, B, N, M, Y = "\033[32m", "\033[31m", "\033[33m", "\033[36m", "\033[1m", "\033[0m", "\033[90m", "\033[35m"

_print_lock = threading.Lock()
_counter = {"done": 0, "total": 0}


def H(extra=None):
    h = {"Authorization": f"Bearer {KEY}"} if KEY else {}
    if extra:
        h.update(extra)
    return h


# Pace requests under the server's ~20 rps per-key limit.
RPS = float(os.environ.get("COVERAGE_RPS", "12"))
_gate = threading.Lock()
_next_at = [0.0]

def _throttle():
    delay = 1.0 / RPS
    with _gate:
        now = time.monotonic()
        t = max(now, _next_at[0])
        _next_at[0] = t + delay
        wait = t - now
    if wait > 0:
        time.sleep(wait)


def _http(method, url, **kw):
    """One choke point for every request: paced by the token gate, retries 429."""
    r = None
    for attempt in range(7):
        _throttle()
        r = getattr(requests, method)(url, **kw)
        if r.status_code == 429:
            ra = r.headers.get("Retry-After")
            time.sleep(float(ra) if ra else min(2.0, 0.3 * (attempt + 1)))
            continue
        return r
    return r  # retries exhausted, hand back the last 429


def api(method, path, data=None, timeout=60):
    r = _http(method, f"{BASE}{path}", json=data, headers=H(), timeout=timeout)
    if not r.ok:
        raise RuntimeError(f"{method.upper()} {path} -> {r.status_code} {r.text[:160]}")
    if r.status_code == 204 or not r.text:
        return None
    return r.json()


def create(template="python-3.11", files=None, setup=None, start=None, port=0, timeout=300):
    body = {"template": template, "timeout": timeout}
    if files: body["files"] = files
    if setup: body["setup"] = setup
    if start: body["start"] = start
    if port: body["port"] = port
    return api("post", "/v1/sandboxes", body)


def execc(sid, cmd, timeout=60):
    return api("post", f"/v1/sandboxes/{sid}/exec", {"cmd": cmd, "timeout": timeout})


def destroy(sid):
    try:
        _http("delete", f"{BASE}/v1/sandboxes/{sid}", headers=H(), timeout=30)
    except Exception:
        pass


def fs_write(sid, path, content):
    r = _http("post", f"{BASE}/v1/sandboxes/{sid}/files", params={"path": path},
              data=content if isinstance(content, bytes) else content.encode(),
              headers=H({"Content-Type": "application/octet-stream"}), timeout=30)
    r.raise_for_status()


def fs_read(sid, path):
    r = _http("get", f"{BASE}/v1/sandboxes/{sid}/files", params={"path": path}, headers=H(), timeout=30)
    r.raise_for_status()
    return r.text


def fs_list(sid, path="/"):
    """The list endpoint wraps entries as {"entries": [...]}."""
    resp = api("get", f"/v1/sandboxes/{sid}/filesystem/list?path={path}")
    return resp.get("entries", []) if isinstance(resp, dict) else (resp or [])


def curl_app(url, want=None, want_status=200, tries=25):
    """Poll a deployed app_url until it responds; verify body/status."""
    last = None
    for _ in range(tries):
        try:
            r = _http("get", url, headers=H(), timeout=5)
            last = r
            if r.status_code == want_status and (want is None or want in r.text):
                return r
        except Exception as e:
            last = e
        time.sleep(0.2)
    raise RuntimeError(f"app not ready/incorrect: {getattr(last,'status_code',last)} body={getattr(last,'text','')[:120]!r}")


def func_register(name, files, start, port=8000, env=None, template="python-3.11"):
    body = {"name": name, "template": template, "files": files, "start": start, "port": port}
    if env: body["env"] = env
    r = _http("post", f"{BASE}/v1/functions", json=body, headers=H(), timeout=30)
    if r.status_code == 409:
        r = _http("put", f"{BASE}/v1/functions/{name}", json=body, headers=H(), timeout=30)
    if not r.ok:
        raise RuntimeError(f"register {name} -> {r.status_code} {r.text[:160]}")


def func_invoke(name, payload, timeout=60):
    r = _http("post", f"{BASE}/v1/functions/{name}/invoke",
              data=json.dumps(payload).encode(),
              headers=H({"Content-Type": "application/json"}), timeout=timeout)
    if not r.ok:
        raise RuntimeError(f"invoke {name} -> {r.status_code} {r.text[:160]}")
    return r.json()


CASES = []  # each: {"cat": str, "name": str, "run": callable() -> str}


def case(cat, name):
    def deco(fn):
        CASES.append({"cat": cat, "name": name, "run": fn})
        return fn
    return deco


def _read_api(path, key=None):
    def run():
        d = api("get", path)
        n = len(d.get(key, [])) if (key and isinstance(d, dict)) else (len(d) if isinstance(d, (list, dict)) else 1)
        return f"{path} ok ({n} items)" if key else f"{path} ok"
    return run

for _p, _k in [("/v1/templates", None), ("/v1/keys", None), ("/v1/usage", None),
               ("/v1/billing", None), ("/v1/builds", None), ("/v1/functions", "functions"),
               ("/v1/sandboxes", "sandboxes")]:
    CASES.append({"cat": "read-api", "name": _p, "run": _read_api(_p, _k)})

CASES.append({"cat": "read-api", "name": "/health",
              "run": lambda: ("ok" if requests.get(f"{BASE}/health", timeout=5).ok else _fail())})


def _fail():
    raise RuntimeError("not ok")


EXEC_CASES = [
    ("py-pow",        "python3 -c 'print(2**10)'",                                  "1024"),
    ("py-json",       "python3 -c 'import json;print(json.dumps({\"a\":1}))'",      '{"a": 1}'),
    ("py-sha256",     "python3 -c 'import hashlib;print(hashlib.sha256(b\"abc\").hexdigest())'", "ba7816bf"),
    ("py-datetime",   "python3 -c 'import datetime;print(datetime.date(2020,1,1).year)'", "2020"),
    ("py-cwd",        "python3 -c 'import os;print(os.getcwd())'",                   "/"),
    ("py-version",    "python3 -c 'import sys;print(sys.version_info[0])'",          "3"),
    ("py-listcomp",   "python3 -c 'print(sum(i*i for i in range(5)))'",             "30"),
    ("py-regex",      "python3 -c 'import re;print(re.findall(r\"[0-9]+\",\"a1b22\"))'", "['1', '22']"),
    ("py-base64",     "python3 -c 'import base64;print(base64.b64encode(b\"hi\").decode())'", "aGk="),
    ("py-math",       "python3 -c 'import math;print(math.factorial(6))'",          "720"),
    ("py-unicode",    "python3 -c 'print(\"h\\u00e9llo\\u2713\")'",                 "héllo✓"),
    ("py-except",     "python3 -c '\ntry:\n 1/0\nexcept ZeroDivisionError:\n print(\"caught\")'", "caught"),
    ("py-sqlite",     "python3 -c 'import sqlite3;c=sqlite3.connect(\":memory:\");c.execute(\"create table t(x)\");c.execute(\"insert into t values(7)\");print(c.execute(\"select sum(x) from t\").fetchone()[0])'", "7"),
    ("py-cpucount",   "python3 -c 'import os;print(1 if os.cpu_count() else 0)'",    "1"),
    ("bash-echo",     "echo hi",                                                    "hi"),
    ("bash-pipe-wc",  "printf 'a\\nb\\nc\\n' | wc -l | tr -d ' '",                  "3"),
    ("bash-grep",     "printf 'x\\ny\\n' | grep y",                                 "y"),
    ("bash-awk",      "echo '1 2 3' | awk '{print $1+$2+$3}'",                      "6"),
    ("bash-uname",    "uname -s",                                                   ""),
    ("bash-envvar",   "FOO=bar sh -c 'echo $FOO'",                                  "bar"),
    ("bash-seq",      "seq 1 5 | tr '\\n' ',' ",                                    "1,2,3,4,5,"),
    ("py-fileio",     "python3 -c 'open(\"t.txt\",\"w\").write(\"ok\");print(open(\"t.txt\").read())'", "ok"),
    ("py-multiline",  "printf 'x=3\\ny=4\\nprint(x*y)\\n' > m.py && python3 m.py",  "12"),
    ("bash-exitcode", "sh -c 'exit 0' && echo passed",                              "passed"),
]
for _n, _cmd, _want in EXEC_CASES:
    def _mk(cmd=_cmd, want=_want):
        def run():
            sid = create()["id"]
            try:
                r = execc(sid, cmd)
                out = (r.get("stdout") or "").strip()
                if r.get("exit_code", 0) != 0:
                    raise RuntimeError(f"exit={r.get('exit_code')} stderr={r.get('stderr','')[:80]}")
                if want and want not in out:
                    raise RuntimeError(f"want {want!r} got {out[:80]!r}")
                return f"stdout={out[:48]!r}"
            finally:
                destroy(sid)
        return run
    CASES.append({"cat": "sandbox-exec", "name": _n, "run": _mk()})


@case("sandbox-exec", "venv-pip-install")
def _venv_pip():
    sid = create()["id"]
    try:
        r = execc(sid, "python3 -m venv .venv && ./.venv/bin/pip -q install requests && "
                        "./.venv/bin/python -c 'import requests;print(requests.__version__)'", timeout=180)
        out = (r.get("stdout") or "").strip()
        if r.get("exit_code", 0) != 0:
            raise RuntimeError(f"exit={r.get('exit_code')} {r.get('stderr','')[:120]}")
        return f"requests {out.splitlines()[-1] if out else '?'}"
    finally:
        destroy(sid)


@case("filesystem", "write-read-text")
def _fs1():
    sid = create()["id"]
    try:
        fs_write(sid, "hello.txt", "world-42")
        if fs_read(sid, "hello.txt") != "world-42":
            raise RuntimeError("roundtrip mismatch")
        return "roundtrip ok"
    finally:
        destroy(sid)

@case("filesystem", "mkdir-and-list")
def _fs2():
    sid = create()["id"]
    try:
        api("post", f"/v1/sandboxes/{sid}/filesystem/mkdir", {"path": "subdir"})
        names = [f.get("name") for f in fs_list(sid, "/")]
        if "subdir" not in names:
            raise RuntimeError(f"subdir not in {names}")
        return f"ls={names[:5]}"
    finally:
        destroy(sid)

@case("filesystem", "nested-write-read")
def _fs3():
    sid = create()["id"]
    try:
        api("post", f"/v1/sandboxes/{sid}/filesystem/mkdir", {"path": "a/b"})
        fs_write(sid, "a/b/c.txt", "deep")
        if fs_read(sid, "a/b/c.txt") != "deep":
            raise RuntimeError("nested mismatch")
        return "nested ok"
    finally:
        destroy(sid)

@case("filesystem", "move-rename")
def _fs4():
    sid = create()["id"]
    try:
        fs_write(sid, "old.txt", "mv")
        api("post", f"/v1/sandboxes/{sid}/filesystem/move", {"src": "old.txt", "dst": "new.txt"})
        if fs_read(sid, "new.txt") != "mv":
            raise RuntimeError("move failed")
        return "moved"
    finally:
        destroy(sid)

@case("filesystem", "remove")
def _fs5():
    sid = create()["id"]
    try:
        fs_write(sid, "gone.txt", "x")
        api("post", f"/v1/sandboxes/{sid}/filesystem/remove", {"path": "gone.txt"})
        try:
            fs_read(sid, "gone.txt"); raise RuntimeError("still present")
        except requests.HTTPError:
            return "removed"
        except RuntimeError as e:
            if "still present" in str(e): raise
            return "removed"
    finally:
        destroy(sid)

@case("filesystem", "stat")
def _fs6():
    sid = create()["id"]
    try:
        fs_write(sid, "s.txt", "12345")
        st = api("get", f"/v1/sandboxes/{sid}/filesystem/stat?path=s.txt")
        return f"stat={ {k: st.get(k) for k in ('name','size') if k in st} }"
    finally:
        destroy(sid)

@case("filesystem", "large-file-roundtrip")
def _fs7():
    sid = create()["id"]
    try:
        blob = "x" * 100_000
        fs_write(sid, "big.txt", blob)
        if len(fs_read(sid, "big.txt")) != 100_000:
            raise RuntimeError("size mismatch")
        return "100KB ok"
    finally:
        destroy(sid)

@case("filesystem", "api-write-exec-reads")
def _fs8():
    sid = create()["id"]
    try:
        fs_write(sid, "bridge.txt", "seen-by-exec")
        out = (execc(sid, "cat bridge.txt").get("stdout") or "").strip()
        if out != "seen-by-exec":
            raise RuntimeError(f"got {out!r}")
        return "api<->exec ok"
    finally:
        destroy(sid)

@case("filesystem", "multi-file-listing")
def _fs9():
    sid = create()["id"]
    try:
        for i in range(5):
            fs_write(sid, f"f{i}.txt", str(i))
        names = [f.get("name") for f in fs_list(sid, "/")]
        if sum(1 for n in names if n and n.startswith("f")) < 5:
            raise RuntimeError(f"missing files: {names}")
        return "5 files listed"
    finally:
        destroy(sid)

@case("filesystem", "binary-bytes")
def _fs10():
    sid = create()["id"]
    try:
        fs_write(sid, "b.bin", bytes(range(32, 127)))
        got = fs_read(sid, "b.bin")
        if len(got) < 90:
            raise RuntimeError("short read")
        return "bytes ok"
    finally:
        destroy(sid)


@case("lifecycle", "create-get")
def _lc1():
    sid = create()["id"]
    try:
        d = api("get", f"/v1/sandboxes/{sid}")
        return f"state={d.get('status', d.get('state'))}"
    finally:
        destroy(sid)

@case("lifecycle", "pause-resume")
def _lc2():
    sid = create()["id"]
    try:
        api("post", f"/v1/sandboxes/{sid}/pause")
        api("post", f"/v1/sandboxes/{sid}/resume")
        return "paused+resumed"
    finally:
        destroy(sid)

@case("lifecycle", "refresh-ttl")
def _lc3():
    sid = create()["id"]
    try:
        d = api("post", f"/v1/sandboxes/{sid}/refresh")
        return f"expires={str(d.get('expires_at',''))[:19]}" if d else "refreshed"
    finally:
        destroy(sid)

@case("lifecycle", "set-timeout")
def _lc4():
    sid = create()["id"]
    try:
        api("post", f"/v1/sandboxes/{sid}/timeout", {"timeout": 600})
        return "timeout set"
    finally:
        destroy(sid)

@case("lifecycle", "metrics")
def _lc5():
    sid = create()["id"]
    try:
        m = api("get", f"/v1/sandboxes/{sid}/metrics")
        return f"metrics keys={list(m.keys())[:4]}"
    finally:
        destroy(sid)

@case("lifecycle", "logs-after-exec")
def _lc6():
    sid = create()["id"]
    try:
        execc(sid, "echo logged")
        api("get", f"/v1/sandboxes/{sid}/logs")
        return "logs served"
    finally:
        destroy(sid)

@case("lifecycle", "destroy-then-404")
def _lc7():
    sid = create()["id"]
    destroy(sid)
    r = requests.get(f"{BASE}/v1/sandboxes/{sid}", headers=H(), timeout=10)
    if r.status_code != 404:
        raise RuntimeError(f"expected 404 got {r.status_code}")
    return "destroyed→404"

@case("lifecycle", "explicit-timeout-short")
def _lc8():
    sid = create(timeout=120)["id"]
    try:
        d = api("get", f"/v1/sandboxes/{sid}")
        return f"created with ttl, exp={str(d.get('expires_at',''))[:19]}"
    finally:
        destroy(sid)


@case("process", "process-list")
def _pr1():
    sid = create()["id"]
    try:
        p = api("get", f"/v1/sandboxes/{sid}/process/list")
        return f"{len(p or [])} procs"
    finally:
        destroy(sid)

@case("process", "process-list-after-bg")
def _pr2():
    sid = create()["id"]
    try:
        execc(sid, "sleep 30 &")
        p = api("get", f"/v1/sandboxes/{sid}/process/list")
        return f"{len(p or [])} procs after bg"
    finally:
        destroy(sid)


def _template_cases():
    try:
        tpls = api("get", "/v1/templates") or []
    except Exception:
        return
    for t in tpls:
        tid = t.get("id") or t.get("name")   # create() needs the template ID, not display name
        tlabel = t.get("name") or tid
        if not tid:
            continue
        def _mk(tid=tid):
            def run():
                sid = create(template=tid)["id"]
                try:
                    d = api("get", f"/v1/sandboxes/{sid}")
                    return f"{tid} → {d.get('status', d.get('state'))}"
                finally:
                    destroy(sid)
            return run
        CASES.append({"cat": "template", "name": tlabel, "run": _mk()})


def _http_handler(body_expr, marker):
    """Build a stdlib http.server whose GET returns `marker` + body_expr eval."""
    return (
        "import os,json\n"
        "from http.server import BaseHTTPRequestHandler, HTTPServer\n"
        "class Hh(BaseHTTPRequestHandler):\n"
        "  def _r(self):\n"
        "    n=int(self.headers.get('Content-Length',0) or 0)\n"
        "    body=self.rfile.read(n).decode() if n else ''\n"
        f"    out={body_expr}\n"
        "    data=out.encode() if isinstance(out,str) else json.dumps(out).encode()\n"
        "    code=404 if self.path=='/__nope__' else 200\n"
        "    self.send_response(code); self.send_header('Content-Type','text/plain')\n"
        "    self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)\n"
        "  do_GET=do_POST=_r\n"
        "  def log_message(self,*a): pass\n"
        "HTTPServer(('0.0.0.0',int(os.environ.get('PORT',8000))),Hh).serve_forever()\n"
    )

WEB_CASES = [
    ("static-marker",  "'MARK-static'",                                  "MARK-static"),
    ("json-response",  "{'ok':True,'kind':'json'}",                      '"kind": "json"'),
    ("echo-post",      "'echo:'+body",                                   "echo:"),
    ("port-env",       "'port='+str(os.environ.get('PORT'))",            "port="),
    ("path-echo",      "'path='+self.path",                              "path=/"),
    ("html-page",      "'<html><body>MARK-html</body></html>'",          "MARK-html"),
    ("compute-fib",    "'fib=%d'%(lambda n:__import__('functools').reduce(lambda a,_:(a[1],a[0]+a[1]),range(n),(0,1))[0])(10)", "fib=55"),
    ("query-echo",     "'q='+self.path.split('?',1)[-1]",                "q="),
    ("headers-count",  "'hdrs=%d'%len(self.headers)",                    "hdrs="),
    ("large-payload",  "'Z'*5000",                                       "ZZZ"),
    ("plain-hello",    "'hello-from-deploy'",                            "hello-from-deploy"),
    ("uuid-page",      "'id='+__import__('uuid').uuid4().hex",           "id="),
]
for _n, _expr, _want in WEB_CASES:
    def _mkweb(name=_n, expr=_expr, want=_want):
        def run():
            files = [{"path": "app.py", "content": _http_handler(expr, want)}]
            resp = create(files=files, start="python3 app.py", port=8000)
            sid = resp.get("id")
            url = resp.get("app_url")
            try:
                if not url:
                    raise RuntimeError(f"no app_url; deploy_log={resp.get('deploy_log')}")
                curl_app(url, want=want)
                return f"served {want!r} @ {url.split('/preview/')[-1]}"
            finally:
                if not KEEP_APPS:
                    destroy(sid)
        return run
    CASES.append({"cat": "web-deploy", "name": _n, "run": _mkweb()})


_FN_HELPERS = (
    "def _fib(n):\n a,b=0,1\n for _ in range(n): a,b=b,a+b\n return a\n"
    "def _isprime(n):\n return n>1 and all(n%i for i in range(2,int(n**0.5)+1))\n"
)

def _eval_expr(expr, inp):
    scope = {"inp": inp, "math": math, "hashlib": hashlib, "base64": base64,
             "uuid": uuid, "codecs": codecs, "re": re, "time": time}
    scope["_fib"] = lambda n: (lambda a=0, b=1: [(a := b, b := a + b) for _ in range(n)] and a)()
    def _fib(n):
        a, b = 0, 1
        for _ in range(n): a, b = b, a + b
        return a
    def _isprime(n): return n > 1 and all(n % i for i in range(2, int(n ** 0.5) + 1))
    scope["_fib"], scope["_isprime"] = _fib, _isprime
    return eval(expr, scope)

# (name, expr over `inp`, sample input, deterministic?)
FN_OPS = [
    ("add10",     "inp.get('x',0)+10",                               {"x": 5},        True),
    ("square",    "inp.get('x',0)**2",                               {"x": 6},        True),
    ("double",    "inp.get('x',0)*2",                                {"x": 21},       True),
    ("negate",    "-inp.get('x',0)",                                 {"x": 8},        True),
    ("factorial", "math.factorial(int(inp.get('x',0)))",             {"x": 6},        True),
    ("fib",       "_fib(int(inp.get('x',0)))",                       {"x": 10},       True),
    ("isprime",   "_isprime(int(inp.get('x',0)))",                   {"x": 17},       True),
    ("reverse",   "str(inp.get('s',''))[::-1]",                      {"s": "abc"},    True),
    ("upper",     "str(inp.get('s','')).upper()",                    {"s": "hey"},    True),
    ("lower",     "str(inp.get('s','')).lower()",                    {"s": "HEY"},    True),
    ("strlen",    "len(str(inp.get('s','')))",                       {"s": "hello"},  True),
    ("sha256",    "hashlib.sha256(str(inp.get('s','')).encode()).hexdigest()", {"s": "abc"}, True),
    ("md5",       "hashlib.md5(str(inp.get('s','')).encode()).hexdigest()",    {"s": "abc"}, True),
    ("base64enc", "base64.b64encode(str(inp.get('s','')).encode()).decode()",  {"s": "hi"},  True),
    ("rot13",     "codecs.encode(str(inp.get('s','')),'rot_13')",    {"s": "hello"},  True),
    ("sumlist",   "sum(inp.get('xs',[]))",                           {"xs": [1,2,3]}, True),
    ("maxlist",   "max(inp.get('xs',[0]))",                          {"xs": [4,9,2]}, True),
    ("sortlist",  "sorted(inp.get('xs',[]))",                        {"xs": [3,1,2]}, True),
    ("wordcount", "len(str(inp.get('s','')).split())",               {"s": "a b c"},  True),
    ("titlecase", "str(inp.get('s','')).title()",                    {"s": "foo bar"},True),
    ("c2f",       "inp.get('c',0)*9/5+32",                           {"c": 100},      True),
    ("palindrome","(lambda s: s==s[::-1])(str(inp.get('s','')))",    {"s": "racecar"},True),
    ("gcd",       "math.gcd(int(inp.get('a',0)),int(inp.get('b',0)))", {"a": 12,"b": 18}, True),
    ("hexop",     "hex(int(inp.get('x',0)))",                        {"x": 255},      True),
    ("binop",     "bin(int(inp.get('x',0)))",                        {"x": 5},        True),
    ("vowels",    "sum(c in 'aeiouAEIOU' for c in str(inp.get('s','')))", {"s": "education"}, True),
    ("slugify",   "re.sub(r'[^a-z0-9]+','-',str(inp.get('s','')).lower()).strip('-')", {"s": "Hello World!"}, True),
    ("uuid",      "uuid.uuid4().hex",                                {},              False),
    ("timenow",   "time.time()",                                     {},              False),
    ("randint",   "__import__('random').randint(1,100)",             {},              False),
]

def _fn_handler(name, expr):
    return (
        "import json,os,math,hashlib,base64,uuid,codecs,re,time,random\n"
        "from http.server import BaseHTTPRequestHandler, HTTPServer\n"
        + _FN_HELPERS +
        "def compute(inp):\n"
        f"  return {expr}\n"
        "class Hh(BaseHTTPRequestHandler):\n"
        "  def _r(self):\n"
        "    n=int(self.headers.get('Content-Length',0) or 0)\n"
        "    raw=self.rfile.read(n).decode() if n else ''\n"
        "    inp=json.loads(raw) if raw else {}\n"
        f"    out=json.dumps({{'result':compute(inp),'fn':'{name}'}}).encode()\n"
        "    self.send_response(200); self.send_header('Content-Type','application/json')\n"
        "    self.send_header('Content-Length',str(len(out))); self.end_headers(); self.wfile.write(out)\n"
        "  do_GET=do_POST=_r\n"
        "  def log_message(self,*a): pass\n"
        "HTTPServer(('0.0.0.0',int(os.environ.get('PORT',8000))),Hh).serve_forever()\n"
    )

def _register_fn_pods():
    for name, expr, _inp, _det in FN_OPS:
        func_register(f"fn-{name}", [{"path": "handler.py", "content": _fn_handler(name, expr)}],
                      start="python3 handler.py", port=8000)

for _n, _expr, _inp, _det in FN_OPS:
    def _mkfn(name=_n, expr=_expr, inp=_inp, det=_det):
        def run():
            r = func_invoke(f"fn-{name}", inp)
            got = r.get("result")
            if det:
                exp = _eval_expr(expr, inp)
                ok = (got == exp) or (isinstance(got, (int, float)) and isinstance(exp, (int, float)) and abs(got - exp) < 1e-6)
                if not ok:
                    raise RuntimeError(f"{name}({inp})={got!r} expected {exp!r}")
                return f"{name} → {got}"
            if got is None:
                raise RuntimeError("null result")
            return f"{name} → {str(got)[:24]}"
        return run
    CASES.append({"cat": "function", "name": _n, "run": _mkfn()})


def run_case(c):
    t0 = time.time()
    try:
        detail = c["run"]()
        ok, msg = True, detail or "ok"
    except Exception as e:
        ok, msg = False, str(e)[:200]
    ms = int((time.time() - t0) * 1000)
    with _print_lock:
        _counter["done"] += 1
        n = _counter["done"]
        tag = f"{G}✓{N}" if ok else f"{R}✗{N}"
        col = C if ok else R
        print(f"  {tag} [{n:>3}/{_counter['total']}] {col}{c['cat']:<13}{N} {c['name']:<24} "
              f"{M}{ms:>5}ms{N}  {msg[:88]}")
    return {"cat": c["cat"], "name": c["name"], "ok": ok, "ms": ms, "msg": msg}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep-apps", action="store_true")
    ap.add_argument("--loop", type=int, default=1)
    ap.add_argument("--workers", type=int, default=8)
    args = ap.parse_args()
    global KEEP_APPS
    KEEP_APPS = args.keep_apps

    if not KEY:
        print(f"{R}PILLDROPS_API_KEY not set{N}", file=sys.stderr); sys.exit(2)
    try:
        if not requests.get(f"{BASE}/health", timeout=5).ok:
            raise RuntimeError("health not ok")
    except Exception as e:
        print(f"{R}server unreachable at {BASE}: {e}{N}", file=sys.stderr); sys.exit(2)

    _template_cases()  # dynamic, depends on live templates

    print(f"\n{B}live coverage{N} → {BASE}   ({len(CASES)} unique use cases, {args.workers} workers)\n")
    print(f"{M}registering {len(FN_OPS)} function pods…{N}")
    _register_fn_pods()

    all_results = []
    for it in range(args.loop):
        if args.loop > 1:
            print(f"\n{B}── pass {it+1}/{args.loop} ──{N}")
        _counter["done"], _counter["total"] = 0, len(CASES)
        results = []
        with ThreadPoolExecutor(max_workers=args.workers) as ex:
            futs = [ex.submit(run_case, c) for c in CASES]
            for f in as_completed(futs):
                results.append(f.result())
        all_results.append(results)
        _summary(results)

    print(f"\n{B}pods left running (scale-to-zero):{N} {len(FN_OPS)} functions "
          f"→ pilldrops function list   |   dashboard: {BASE}/dashboard")


def _summary(results):
    by_cat = {}
    for r in results:
        d = by_cat.setdefault(r["cat"], {"ok": 0, "n": 0})
        d["n"] += 1
        d["ok"] += 1 if r["ok"] else 0
    total_ok = sum(1 for r in results if r["ok"])
    print(f"\n{B}── coverage summary ──{N}")
    for cat in sorted(by_cat):
        d = by_cat[cat]
        col = G if d["ok"] == d["n"] else (O if d["ok"] else R)
        print(f"  {col}{cat:<14}{N} {d['ok']:>2}/{d['n']:<2}")
    fails = [r for r in results if not r["ok"]]
    if fails:
        print(f"\n  {R}failures:{N}")
        for r in fails:
            print(f"    {R}✗{N} {r['cat']}/{r['name']}: {r['msg'][:110]}")
    col = G if total_ok == len(results) else O
    print(f"\n  {B}TOTAL: {col}{total_ok}/{len(results)} passed{N}")


if __name__ == "__main__":
    KEEP_APPS = False
    main()

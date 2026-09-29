"""Lifecycle contracts: TTL cleanup, OOM containment, template catalog, reconnect, ps, pause/resume, OOM and crash events."""

import json
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from pdlib import PDDriver, _run


@pytest.mark.guard
@pytest.mark.slow
def test_ttl_auto_cleanup():
    sb = PDDriver(template="python-3.11", timeout=30)
    try:
        time.sleep(55)
        r = _run(["pilldrops", "--json", "sandbox", "get", sb.sandbox_id], timeout=30)
        assert r.returncode != 0 or '"destroyed"' in r.stdout or "not found" in (
            r.stdout + r.stderr
        ), f"sandbox survived TTL+25s: {r.stdout[:120]}"
    finally:
        sb.destroy()  # no-op if already reaped


@pytest.mark.guard
def test_oom_kills_process_not_vm(sandbox):
    sandbox.exec("echo oom-marker > /tmp/oom-guard.txt")
    r = sandbox.exec(
        'python3 -c "b = bytearray()\n'
        'while True: b.extend(b\'x\' * (64*1024*1024))"',
        timeout=120,
    )
    assert r.exit_code != 0
    time.sleep(5)
    r2 = sandbox.exec("cat /tmp/oom-guard.txt && echo post-oom-exec-ok")
    assert "oom-marker" in r2.stdout, "VM rebooted/tmp wiped after OOM (defect #8 regression)"
    assert "post-oom-exec-ok" in r2.stdout


@pytest.mark.guard
@pytest.mark.slow
def test_all_catalog_templates_create():
    r = _run(["pilldrops", "--json", "templates"], timeout=30)
    assert r.returncode == 0
    template_ids = [t["id"] for t in json.loads(r.stdout)]
    assert len(template_ids) >= 20, f"catalog shrank: {template_ids}"

    failures = []

    def probe(tid):
        c = _run(["pilldrops", "--json", "sandbox", "create", "--template", tid,
                  "--timeout", "120"], timeout=120)
        if c.returncode != 0:
            failures.append((tid, (c.stdout + c.stderr)[:150]))
            return
        sid = json.loads(c.stdout)["id"]
        _run(["pilldrops", "sandbox", "rm", sid], timeout=60)

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(probe, template_ids))

    assert not failures, f"templates failing to create: {failures}"


@pytest.mark.guard
def test_reconnect_by_id(sandbox):
    sandbox.exec("echo reconnect-state > /tmp/reconnect.txt")
    r = subprocess.run(
        ["pilldrops", "exec", sandbox.sandbox_id, "--", "bash", "-lc",
         "cat /tmp/reconnect.txt"],
        capture_output=True, text=True, timeout=60,
    )
    assert "reconnect-state" in r.stdout


@pytest.mark.guard
def test_ps_on_firecracker(sandbox):
    r = _run(["pilldrops", "--json", "ps", sandbox.sandbox_id], timeout=30)
    assert r.returncode == 0, f"ps failed: {r.stderr[:120]}"
    procs = json.loads(r.stdout)
    assert isinstance(procs, list) and len(procs) >= 1, f"no procs: {r.stdout[:120]}"
    # PID 1 (init/sleep) is always present.
    assert any(p.get("pid") == 1 for p in procs), f"no pid 1 in {procs[:5]}"


@pytest.mark.guard
@pytest.mark.slow
def test_pause_resume_preserves_memory(sandbox):
    """A marker in /dev/shm (RAM-backed) survives only if guest memory was snapshotted, not cold-rebooted."""
    marker = f"ct-e2-{int(time.time())}"
    sandbox.exec(f"echo {marker} > /dev/shm/ct_e2")
    sandbox.pause()
    sandbox.resume()
    r = sandbox.exec("cat /dev/shm/ct_e2")
    assert marker in r.stdout, (
        f"in-RAM marker lost across pause/resume — cold reboot, not memory restore: {r.stdout[:120]}"
    )


@pytest.mark.guard
@pytest.mark.slow
def test_pause_resume_report_their_duration(sandbox):
    """pause and resume answer paused_ms / resume_ms as integers > 0 on a real cycle."""
    r = _run(["pilldrops", "--json", "sandbox", "pause", sandbox.sandbox_id], timeout=120)
    assert r.returncode == 0, f"pause failed: {r.stderr[:120]}"
    paused = json.loads(r.stdout)
    r = _run(["pilldrops", "--json", "sandbox", "resume", sandbox.sandbox_id], timeout=120)
    assert r.returncode == 0, f"resume failed: {r.stderr[:120]}"
    resumed = json.loads(r.stdout)
    assert isinstance(paused.get("paused_ms"), int) and paused["paused_ms"] > 0, f"paused_ms missing or 0: {paused}"
    assert isinstance(resumed.get("resume_ms"), int) and resumed["resume_ms"] > 0, f"resume_ms missing or 0: {resumed}"
    # Positive control: the sandbox is running afterwards.
    assert "ok" in sandbox.exec("echo ok").stdout


_CT_E3_SERVER = (
    "import http.server, socketserver\n"
    "c = [0]\n"
    "class H(http.server.BaseHTTPRequestHandler):\n"
    "    def do_GET(self):\n"
    "        c[0] += 1\n"
    "        self.send_response(200); self.end_headers()\n"
    "        self.wfile.write(str(c[0]).encode())\n"
    "    def log_message(self, *a): pass\n"
    "socketserver.TCPServer(('127.0.0.1', 7777), H).serve_forever()\n"
)


@pytest.mark.guard
@pytest.mark.slow
def test_running_server_survives_pause(sandbox):
    """The in-memory hit counter must keep counting across the cycle, proving the process was frozen, not restarted."""
    sandbox.write_file("/tmp/ct_e3_srv.py", _CT_E3_SERVER)
    sandbox.exec("nohup python3 /tmp/ct_e3_srv.py >/tmp/ct_e3.log 2>&1 &")
    first = None
    for _ in range(15):
        out = sandbox.exec("curl -s localhost:7777 || true").stdout.strip()
        if out.isdigit():
            first = int(out)
            break
        time.sleep(1)
    assert first is not None, "server never came up before pause"
    sandbox.pause()
    sandbox.resume()
    out = sandbox.exec("curl -s localhost:7777 || true").stdout.strip()
    assert out.isdigit() and int(out) > first, (
        "in-memory counter did not survive resume — server restarted, not frozen "
        f"(before={first}, after={out!r})"
    )


@pytest.mark.guard
@pytest.mark.slow
def test_oom_produces_visible_event(sandbox):
    """An OOM kill inside a sandbox must surface as an oom event on both runtimes."""
    sandbox.exec(
        'python3 -c "b = bytearray()\n'
        'while True: b.extend(b\'x\' * (64*1024*1024))"',
        timeout=60,
    )
    # daemon OOM scanner runs every ~8 s; add fleet-proxy + margin.
    deadline = time.time() + 45
    while time.time() < deadline:
        r = _run(["pilldrops", "--json", "events", sandbox.sandbox_id], timeout=30)
        if r.returncode == 0:
            try:
                events = json.loads(r.stdout or "[]")
            except json.JSONDecodeError:
                events = []
            if any(e.get("event") == "oom" for e in events):
                return
        time.sleep(5)
    raise AssertionError("no 'oom' event appeared within 45 s of an OOM kill")


@pytest.mark.guard
@pytest.mark.slow
def test_registered_server_crash_event(sandbox):
    """The daemon detects the up-to-down edge on a registered server's port and emits a crash event."""
    sandbox.start("python3 -m http.server $PORT", port=8000)
    time.sleep(14)  # let the scanner observe the server up (arms the edge detector)
    sandbox.exec("pkill -f http.server || true")
    deadline = time.time() + 45  # scanner cadence 8s + fleet-proxy + margin
    while time.time() < deadline:
        r = _run(["pilldrops", "--json", "events", sandbox.sandbox_id], timeout=30)
        if r.returncode == 0:
            try:
                events = json.loads(r.stdout or "[]")
            except json.JSONDecodeError:
                events = []
            if any(e.get("event") == "crash" for e in events):
                return
        time.sleep(5)
    raise AssertionError("no 'crash' event appeared after the registered server was killed")

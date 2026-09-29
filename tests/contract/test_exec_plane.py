"""Exec-plane contracts: session env, background processes, resource limits, per-command env.
The parity tests encode known gaps and fail until the platform closes them.
"""

import json
import time

import pytest

from pdlib import _run


@pytest.mark.parity
def test_exec_env_is_full(sandbox):
    """Exec sessions must carry a real user environment, not envd's init env."""
    r = sandbox.exec('echo "HOME=$HOME USER=$USER"; echo "PATH=$PATH"')
    assert r.exit_code == 0
    assert "HOME=/root" in r.stdout, f"bare HOME (D-12): {r.stdout!r}"
    assert "USER=root" in r.stdout, f"empty USER (D-12): {r.stdout!r}"
    assert "/usr/local/bin" in r.stdout


@pytest.mark.parity
def test_go_template_works_naively(sandbox_factory):
    sb = sandbox_factory(template="go-1.22")
    r = sb.exec("go version")
    assert r.exit_code == 0 and "go1.22" in r.stdout, (
        f"go off-PATH (D-12): rc={r.exit_code} out={r.stdout!r} err={r.stderr!r}"
    )


@pytest.mark.parity
def test_rust_template_works_naively(sandbox_factory):
    sb = sandbox_factory(template="rust-1.77")
    r = sb.exec("rustc --version && cargo --version")
    assert r.exit_code == 0 and "rustc 1.77" in r.stdout, (
        f"rust off-PATH (D-12): rc={r.exit_code} out={r.stdout!r} err={r.stderr!r}"
    )


@pytest.mark.parity
def test_nohup_server_survives_session_end(sandbox):
    sandbox.exec(
        "cd /tmp && nohup python3 -m http.server 8123 >/tmp/s.log 2>&1 & echo spawned"
    )
    time.sleep(4)
    r = sandbox.exec("curl -s -o /dev/null -w '%{http_code}' --max-time 5 localhost:8123")
    assert r.stdout.strip() == "200", (
        f"bg server not serving after session end (D-13): {r.stdout!r} log-hint: "
        + sandbox.exec("cat /tmp/s.log 2>/dev/null | tail -3").stdout
    )


@pytest.mark.parity
def test_background_worker_keeps_working(sandbox):
    """The worker must keep doing work across sessions, not merely stay alive."""
    sandbox.exec(
        'nohup bash -c "while true; do date +%s >> /tmp/ticks.txt; sleep 1; done" '
        ">/dev/null 2>&1 & echo spawned"
    )
    time.sleep(5)
    r1 = sandbox.exec("wc -l < /tmp/ticks.txt 2>/dev/null || echo 0")
    time.sleep(3)
    r2 = sandbox.exec("wc -l < /tmp/ticks.txt 2>/dev/null || echo 0")
    t1, t2 = int(r1.stdout.strip() or 0), int(r2.stdout.strip() or 0)
    assert t1 >= 2, f"worker never ran (D-13): ticks={t1}"
    assert t2 > t1, f"worker frozen after session end (D-13): {t1} -> {t2}"


@pytest.mark.guard
def test_exec_detach_returns_pid_immediately(sandbox):
    t0 = time.time()
    r = _run(["pilldrops", "--json", "exec", sandbox.sandbox_id, "--detach",
              "--", "sh", "-c", "sleep 60"], timeout=30)
    elapsed = time.time() - t0
    assert r.returncode == 0, f"detach exec failed: {r.stderr[:120]}"
    out = json.loads(r.stdout)
    assert out.get("pid", 0) > 0, f"no pid returned from --detach: {out}"
    assert elapsed < 15, f"--detach blocked {elapsed:.1f}s (should return immediately)"
    ps = json.loads(_run(["pilldrops", "--json", "ps", sandbox.sandbox_id], timeout=30).stdout)
    assert any("sleep" in (p.get("cmd") or "") for p in ps), (
        f"detached process not running in {[p.get('cmd') for p in ps][:8]}"
    )


# The documented per-exec nproc limit.
NPROC_CONTRACT = 2048


@pytest.mark.parity
def test_exec_limits_actually_apply(sandbox):
    r = sandbox.exec("ulimit -u")
    got = r.stdout.strip()
    assert got.isdigit() and int(got) == NPROC_CONTRACT, (
        f"nproc limit not enforced (D-14): ulimit -u = {got!r}, contract {NPROC_CONTRACT}"
    )


@pytest.mark.guard
def test_limits_do_not_break_pip(sandbox):
    """A 512 MB address-space cap kills pip; whatever limits apply must stay above that."""
    r = sandbox.exec(
        "pip install -q requests 2>&1 | tail -1; "
        "python3 -c \"import requests; print('pip-ok', requests.__version__)\"",
        timeout=240,
    )
    assert "pip-ok" in r.stdout


@pytest.mark.guard
def test_per_command_env_flag(sandbox):
    import subprocess

    r = subprocess.run(
        ["pilldrops", "exec", sandbox.sandbox_id, "--env", "CENV=per-cmd-42",
         "--", "bash", "-lc", "echo CENV=[$CENV]"],
        capture_output=True, text=True, timeout=60,
    )
    assert r.returncode == 0 and "CENV=[per-cmd-42]" in r.stdout, (
        f"exec --env unsupported (S7): rc={r.returncode} out={r.stdout!r} err={r.stderr!r}"
    )


@pytest.mark.guard
def test_env_at_create_injected(sandbox_factory):
    sb = sandbox_factory(template="python-3.11", envs={"GUARD_ENV": "still-injected"})
    r = sb.exec("echo GUARD_ENV=[$GUARD_ENV]")
    assert "GUARD_ENV=[still-injected]" in r.stdout


@pytest.mark.guard
def test_setsid_background_survives(sandbox):
    """setsid is the documented workaround for background processes and must keep working."""
    sandbox.exec(
        "cd /tmp && setsid nohup python3 -m http.server 8124 >/tmp/s2.log 2>&1 & echo up"
    )
    time.sleep(4)
    r = sandbox.exec("curl -s -o /dev/null -w '%{http_code}' --max-time 5 localhost:8124")
    assert r.stdout.strip() == "200"

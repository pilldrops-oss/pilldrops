"""Latency budgets: create, exec, and file-write p50 <= 1.5 s, 10-way concurrent create wall <= 5 s.
Exec pays a fresh CLI process plus TLS handshake per call (~0.6 s floor), so its budget guards the CLI ceiling, not the SDK path.
"""

import statistics
import time
from concurrent.futures import ThreadPoolExecutor

import pytest

from pdlib import PDDriver

RUNS = 5


def _p50(samples):
    return statistics.median(samples)


@pytest.mark.guard
@pytest.mark.slow
def test_create_latency_budget():
    """Pause between creates so the warm pool refills; this measures steady state, not a burst."""
    samples = []
    for _ in range(RUNS):
        sb = PDDriver(template="python-3.11", timeout=180)
        samples.append(sb.create_dur)
        sb.destroy()
        time.sleep(2)
    p50 = _p50(samples)
    print(f"\ncreate p50={p50:.2f}s samples={[f'{s:.2f}' for s in samples]}")
    assert p50 <= 1.5, f"create p50 {p50:.2f}s > 1.5s budget (pool not warm for this template?)"


@pytest.mark.guard
@pytest.mark.slow
def test_exec_latency_budget(sandbox):
    samples = []
    for _ in range(RUNS):
        t0 = time.time()
        r = sandbox.exec("true")
        assert r.exit_code == 0
        samples.append(time.time() - t0)
    p50 = _p50(samples)
    print(f"\nexec p50={p50:.2f}s samples={[f'{s:.2f}' for s in samples]}")
    assert p50 <= 1.5, f"exec p50 {p50:.2f}s > 1.5s CLI ceiling"


@pytest.mark.guard
@pytest.mark.slow
def test_file_write_latency_budget(sandbox):
    samples = []
    for i in range(RUNS):
        t0 = time.time()
        sandbox.write_file(f"/tmp/lat-{i}.txt", "x" * 1024)
        samples.append(time.time() - t0)
    p50 = _p50(samples)
    print(f"\nfile-write p50={p50:.2f}s samples={[f'{s:.2f}' for s in samples]}")
    assert p50 <= 1.5, f"file write p50 {p50:.2f}s > 1.5s budget"


@pytest.mark.guard
@pytest.mark.slow
def test_concurrent_create_wall_budget():
    def one(_i):
        try:
            return PDDriver(template="python-3.11", timeout=180)
        except Exception:
            return None

    t0 = time.time()
    with ThreadPoolExecutor(max_workers=10) as pool:
        boxes = list(pool.map(one, range(10)))
    wall = time.time() - t0
    ok = [b for b in boxes if b]
    for b in ok:
        b.destroy()
    print(f"\n10-concurrent wall={wall:.2f}s ok={len(ok)}/10")
    assert len(ok) == 10, f"only {len(ok)}/10 concurrent creates succeeded"
    assert wall <= 5.0, f"10-concurrent wall {wall:.2f}s > 5s budget"

"""Resource honesty: the specs the platform advertises for a sandbox must match what the guest sees.
The advertised source is pilldrops metrics (vcpu, mem_mib), which echo the daemon's allocation record, not guest telemetry.
"""

import json
import re
import subprocess

import pytest


def _advertised_resources(sandbox_id):
    """Returns (vcpu, mem_mib, detail); detail is the raw metrics JSON or an error description."""
    r = subprocess.run(
        ["pilldrops", "--json", "metrics", sandbox_id],
        capture_output=True, text=True, timeout=30,
    )
    if r.returncode != 0:
        return None, None, f"metrics exit {r.returncode}: {r.stdout} {r.stderr}"
    try:
        data = json.loads(r.stdout)
    except ValueError:
        return None, None, f"metrics returned non-JSON: {r.stdout!r}"
    return data.get("vcpu"), data.get("mem_mib"), r.stdout


def _guest_observed(sandbox):
    """Under Docker nproc and free see the host, so the cgroup v2 limits are authoritative;
    under Firecracker the cgroup reports max and nproc/free -m are."""
    r = sandbox.exec(
        "cat /sys/fs/cgroup/cpu.max 2>/dev/null; echo ---; "
        "cat /sys/fs/cgroup/memory.max 2>/dev/null; echo ---; nproc; echo ---; free -m"
    )
    assert r.exit_code == 0, (
        f"guest probe failed (exit {r.exit_code}): stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    cpu_max, mem_max, nproc_out, free_out = (p.strip() for p in r.stdout.split("---"))
    quota = cpu_max.split()
    if len(quota) == 2 and quota[0] != "max":
        got_cpu = int(quota[0]) / int(quota[1])  # Docker: CFS quota/period
    else:
        assert nproc_out.isdigit(), f"nproc gave no number: {r.stdout!r}"
        got_cpu = int(nproc_out)
    if mem_max.isdigit():
        got_ram_mb = int(mem_max) // (1024 * 1024)  # Docker: memory.max bytes
    else:
        m = re.search(r"Mem:\s+(\d+)", free_out)
        assert m, f"free -m unparseable: {free_out!r}"
        got_ram_mb = int(m.group(1))
    return got_cpu, got_ram_mb


@pytest.mark.guard
def test_advertised_resources_match_guest(sandbox):
    meta = sandbox.meta
    adv_vcpu, adv_ram_mb, metrics_raw = _advertised_resources(sandbox.sandbox_id)
    assert adv_vcpu and adv_ram_mb, (
        f"no advertised resources found for {sandbox.sandbox_id} "
        f"(metrics={metrics_raw}; create response={meta})"
    )

    got_cpu, got_ram_mb = _guest_observed(sandbox)

    where = f"sandbox {sandbox.sandbox_id} on node {meta.get('node')}"
    assert got_cpu == int(adv_vcpu), (
        f"{where}: advertised {adv_vcpu} vCPU, guest sees {got_cpu} (metrics={metrics_raw})"
    )
    # Kernel reservations shave a roughly fixed ~40MB, so 90% is safe for >=1GiB allocations.
    assert got_ram_mb >= int(adv_ram_mb) * 0.9, (
        f"{where}: advertised {adv_ram_mb}MB RAM, guest sees {got_ram_mb}MB (metrics={metrics_raw})"
    )

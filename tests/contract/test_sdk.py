"""SDK contract: the full agent loop through the Python SDK only, no CLI.
Skips when the pilldrops package is missing or predates run(env=), connect, list, and host.
"""

import time

import pytest

pd = pytest.importorskip("pilldrops")


def _has_ws6_surface():
    Sandbox = getattr(pd, "Sandbox", None)
    return bool(Sandbox) and all(
        hasattr(Sandbox, m) for m in ("connect", "list", "host")
    )


@pytest.mark.parity
@pytest.mark.slow
@pytest.mark.skipif(not _has_ws6_surface(),
                    reason="installed pilldrops SDK predates the WS6 surface (needs v1.1.0)")
def test_sdk_full_loop():
    from pilldrops import Sandbox

    sb = Sandbox.create(template="python-3.11", timeout=120)
    try:
        r = sb.run("echo V=$WS6", env={"WS6": "sdk-ok"})
        assert r.ok and "sdk-ok" in r.stdout, f"run(env=) failed: {r.stdout!r}"

        sb.files.write("/tmp/sdk.txt", "sdk-file-ok")
        assert "sdk-file-ok" in sb.files.read("/tmp/sdk.txt")

        assert str(sb.id) in sb.host(3000)

        sb2 = pd.Sandbox.connect(sb.id)
        assert sb2.run("echo reconnect-ok").stdout.strip() == "reconnect-ok"

        assert any(x.get("id") == sb.id for x in pd.Sandbox.list())

        assert "mem_mib" in sb.metrics()
    finally:
        sb.kill()
        time.sleep(0.2)

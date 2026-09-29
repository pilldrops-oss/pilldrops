"""Preview-edge contracts: readiness signals, multi-port, WebSocket upgrade, private by default."""

import socket
import ssl
import time

import certifi
import pytest
import requests

from pdlib import sandbox_domain


@pytest.mark.guard
def test_unready_preview_is_503_json_for_agents(sandbox_with_port):
    """sleep 3600 holds the port registration without ever binding 8000."""
    sandbox_with_port.start("sleep 3600", port=8000)
    time.sleep(3)
    r = requests.get(sandbox_with_port.preview_url(8000), timeout=20)  # Accept: */*
    assert r.status_code == 503, f"false-{r.status_code} for not-ready app (D-9)"
    assert r.headers.get("Retry-After"), "missing Retry-After on 503"
    assert "application/json" in r.headers.get("Content-Type", ""), \
        f"agent got {r.headers.get('Content-Type')!r}, want JSON"
    assert r.json().get("state") == "starting", f"body = {r.text!r}"


@pytest.mark.guard
def test_unready_preview_is_html_page_for_browsers(sandbox_with_port):
    sandbox_with_port.start("sleep 3600", port=8000)
    time.sleep(3)
    r = requests.get(sandbox_with_port.preview_url(8000), timeout=20,
                     headers={"Accept": "text/html"})
    assert r.status_code == 503, f"browser got {r.status_code}, want honest 503"
    assert r.headers.get("Retry-After"), "missing Retry-After for browser"
    assert "Starting your app" in r.text, "browser should get the friendly HTML page"


@pytest.mark.guard
@pytest.mark.slow
def test_multi_port_preview(sandbox_with_port):
    """Additional guest ports are reachable on the preview URL with no registration step."""
    sb = sandbox_with_port
    sb.start("cd /tmp && echo app-8000 > i.html && python3 -m http.server 8000", port=8000)
    sb.exec("mkdir -p /tmp/b && echo app-3000 > /tmp/b/i.html && "
            "cd /tmp/b && setsid nohup python3 -m http.server 3000 >/dev/null 2>&1 & echo up")
    time.sleep(6)

    r8 = requests.get(sb.preview_url(8000) + "/i.html", timeout=20)
    r3 = requests.get(sb.preview_url(3000) + "/i.html", timeout=20)
    assert r8.status_code == 200 and "app-8000" in r8.text, f"8000 → {r8.status_code} {r8.text[:60]!r}"
    assert r3.status_code == 200 and "app-3000" in r3.text, (
        f"second port 3000 not reachable zero-config (S25): {r3.status_code} {r3.text[:60]!r}"
    )


@pytest.mark.guard
@pytest.mark.slow
def test_websocket_upgrade_through_edge(sandbox_with_port):
    sandbox_with_port.exec("pip install -q websockets 2>&1 | tail -1", timeout=240)
    sandbox_with_port.start(
        'python3 -c "'
        "import asyncio, websockets, os\n"
        "async def echo(ws):\n"
        "    async for m in ws: await ws.send('echo:'+m)\n"
        "async def main():\n"
        "    async with websockets.serve(echo, '0.0.0.0', int(os.environ.get('PORT',8000))):\n"
        "        await asyncio.Future()\n"
        'asyncio.run(main())"',
        port=8000,
    )
    time.sleep(8)

    host = f"8000-{sandbox_with_port.sandbox_id}.{sandbox_domain()}"
    req = (
        f"GET / HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\n"
        "Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
        "Sec-WebSocket-Version: 13\r\n\r\n"
    )
    # macOS system Python lacks root certs, so use the certifi bundle.
    ctx = ssl.create_default_context(cafile=certifi.where())
    with socket.create_connection((host, 443), timeout=20) as raw:
        with ctx.wrap_socket(raw, server_hostname=host) as s:
            s.sendall(req.encode())
            first_line = s.recv(1024).decode(errors="replace").splitlines()[0]
    assert "101" in first_line, f"WS upgrade regressed (defect #5): {first_line!r}"


@pytest.mark.guard
def test_registered_server_preview_serves(sandbox_with_port):
    sandbox_with_port.exec("echo preview-marker > /tmp/www/index.html 2>/dev/null || "
                           "(mkdir -p /tmp/www && echo preview-marker > /tmp/www/index.html)")
    sandbox_with_port.start(
        "cd /tmp/www && python3 -m http.server ${PORT:-8000}", port=8000
    )
    deadline = time.time() + 45
    last = None
    while time.time() < deadline:
        try:
            r = requests.get(sandbox_with_port.preview_url(8000) + "/index.html",
                             timeout=10)
            last = f"{r.status_code} {r.text[:60]!r}"
            if r.status_code == 200 and "preview-marker" in r.text:
                return
        except requests.RequestException as e:  # edge may briefly 502 while booting
            last = str(e)[:80]
        time.sleep(3)
    pytest.fail(f"registered-server preview never served: {last}")


@pytest.mark.guard
def test_preview_private_by_default(sandbox):
    url = f"https://8000-{sandbox.sandbox_id}.{sandbox_domain()}/"
    r = requests.get(url, timeout=20, headers={"User-Agent": "Mozilla/5.0 (contract)"})
    assert r.status_code == 403, (
        f"default-visibility preview served HTTP {r.status_code} — "
        f"private-by-default gate is not enforcing: {r.text[:120]}"
    )

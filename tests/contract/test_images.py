"""Image and template contracts: rich default image, toolchain templates, catalog honesty.
The default template must ship Node, npm, procps, and zip without an install step; parity tests fail on the thin image.
"""

import json

import pytest

from pdlib import _run


@pytest.mark.parity
def test_default_image_has_node(sandbox):
    r = sandbox.exec('node -e "console.log(process.version)"')
    assert r.exit_code == 0 and r.stdout.strip().startswith("v"), (
        f"no Node in default image (S2): rc={r.exit_code} {r.stderr!r}"
    )
    # Node 20 is the floor current create-vite and vitest need.
    major = int(r.stdout.strip().lstrip("v").split(".")[0])
    assert major >= 20, f"Node too old: {r.stdout.strip()}"


@pytest.mark.parity
def test_default_image_npm_install(sandbox):
    r = sandbox.exec(
        "cd /tmp && npm init -y >/dev/null 2>&1 && "
        "npm install --no-fund --no-audit lodash >/dev/null 2>&1 && "
        "node -e \"console.log('npm-ok', require('lodash').chunk([1,2,3,4],2).length)\"",
        timeout=180,
    )
    assert "npm-ok 2" in r.stdout, f"npm install broken (S12): {r.stdout!r} {r.stderr!r}"


@pytest.mark.parity
def test_default_image_has_procps_and_zip(sandbox):
    r = sandbox.exec(
        "for t in ps free pkill pgrep zip unzip jq; do "
        "command -v $t >/dev/null && echo \"$t=ok\" || echo \"$t=MISSING\"; done"
    )
    missing = [line.split("=")[0] for line in r.stdout.splitlines() if "MISSING" in line]
    assert not missing, f"default image missing tools (S9): {missing}"


@pytest.mark.guard
def test_default_image_keeps_python(sandbox):
    r = sandbox.exec("python3 -V && pip --version | cut -d' ' -f1-2")
    assert "Python 3.11" in r.stdout and "pip" in r.stdout


@pytest.mark.guard
def test_go_template(sandbox_factory):
    sb = sandbox_factory(template="go-1.22")
    r = sb.exec("go version")
    assert r.exit_code == 0 and "go1.22" in r.stdout


@pytest.mark.guard
def test_rust_template(sandbox_factory):
    sb = sandbox_factory(template="rust-1.77")
    r = sb.exec("rustc --version && cargo --version")
    assert r.exit_code == 0 and "rustc 1.77" in r.stdout


@pytest.mark.parity
@pytest.mark.slow
def test_playwright_template_node_oneliner(sandbox_factory):
    """The playwright template ships browsers and the node driver, so a plain node script works with zero install."""
    sb = sandbox_factory(template="playwright", timeout=600)
    sb.write_file("/root/pw.mjs", (
        'import { chromium } from "playwright";\n'
        'const b = await chromium.launch({args:["--no-sandbox"]});\n'
        'const p = await b.newPage(); await p.goto("https://example.com");\n'
        'console.log("title:", await p.title());\n'
        'await p.screenshot({path:"/tmp/shot.png"}); await b.close();\n'
        'console.log("shot-bytes", (await import("fs")).readFileSync("/tmp/shot.png").length);\n'
    ))
    r = sb.exec("cd /root && node pw.mjs 2>&1", timeout=120)
    assert "Example Domain" in r.stdout and "shot-bytes" in r.stdout, (
        f"playwright node one-liner failed (S18): {r.stdout[-300:]!r}"
    )


@pytest.mark.parity
def test_catalog_ram_matches_reality(sandbox):
    r = _run(["pilldrops", "--json", "templates"], timeout=30)
    catalog = {t["id"]: t for t in json.loads(r.stdout)}
    # python-3.11 resolves to code-interpreter; find whichever id the catalog lists.
    entry = catalog.get("code-interpreter") or catalog.get("python-3.11")
    assert entry, "code-interpreter/python-3.11 not in catalog"
    advertised = entry["memory_mb"]

    m = sandbox.exec("grep MemTotal /proc/meminfo | awk '{print $2}'")  # kB
    actual_mb = int(m.stdout.strip()) / 1024
    ratio = actual_mb / advertised
    assert 0.90 <= ratio <= 1.10, (
        f"catalog RAM {advertised}MB vs actual {actual_mb:.0f}MB (ratio {ratio:.2f})"
    )

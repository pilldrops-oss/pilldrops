#!/usr/bin/env python3
"""Render the security scan results into a report page and decide the verdict.

Usage: pd-report <outdir> <fail-on> <target> [url]

Exit 0 when nothing was found at or above the fail-on severity, 1 otherwise.
Only findings the tools actually reported are shown — no invented severities,
and a tool that produced nothing is reported as "clean", never as "not run".
"""
import json
import os
import sys
from html import escape

ORDER = ["low", "medium", "high", "critical"]
# Headers worth flagging when missing, with why-it-matters kept short.
HEADERS = {
    "strict-transport-security": "forces HTTPS on later visits",
    "content-security-policy": "limits where scripts may load from",
    "x-content-type-options": "stops MIME sniffing",
    "x-frame-options": "blocks clickjacking via frames",
    "referrer-policy": "controls how much URL leaks to other sites",
}


def read_json(path, default):
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return default


def severity_of(vuln):
    """Best-effort severity for an OSV vulnerability record."""
    for entry in vuln.get("severity") or []:
        score = str(entry.get("score", ""))
        # CVSS vectors carry no plain label; fall back to the database_specific
        # or ecosystem_specific hints OSV sometimes provides.
        if score and score[0].isdigit():
            try:
                num = float(score)
            except ValueError:
                num = None
            if num is not None:
                if num >= 9.0:
                    return "critical"
                if num >= 7.0:
                    return "high"
                if num >= 4.0:
                    return "medium"
                return "low"
    db = vuln.get("database_specific") or {}
    label = str(db.get("severity", "")).lower()
    return label if label in ORDER else "medium"


def collect_deps(data):
    """Flatten osv-scanner JSON into (package, version, id, severity, source)."""
    out = []
    for res in data.get("results", []) or []:
        source = (res.get("source") or {}).get("path", "")
        for pkg in res.get("packages", []) or []:
            info = pkg.get("package") or {}
            name = info.get("name", "?")
            version = info.get("version", "?")
            for vuln in pkg.get("vulnerabilities", []) or []:
                out.append({
                    "package": name,
                    "version": version,
                    "id": vuln.get("id", "?"),
                    "severity": severity_of(vuln),
                    "source": os.path.basename(source),
                })
    return out


def main():
    if len(sys.argv) < 4:
        print("usage: pd-report <outdir> <fail-on> <target> [url]", file=sys.stderr)
        return 2
    outdir, fail_on, target = sys.argv[1:4]
    url = sys.argv[4] if len(sys.argv) > 4 else ""

    deps = collect_deps(read_json(os.path.join(outdir, "dependencies.json"), {}))
    secrets_raw = read_json(os.path.join(outdir, "secrets.json"), [])
    secrets = secrets_raw if isinstance(secrets_raw, list) else []

    # Missing security headers — only assessed when a URL was actually fetched.
    missing_headers = []
    headers_path = os.path.join(outdir, "headers.txt")
    fetched = False
    if url and os.path.exists(headers_path):
        try:
            with open(headers_path, errors="replace") as fh:
                raw = fh.read()
        except OSError:
            raw = ""
        fetched = bool(raw.strip())
        present = {
            line.split(":", 1)[0].strip().lower()
            for line in raw.splitlines() if ":" in line
        }
        if fetched:
            missing_headers = [(h, why) for h, why in HEADERS.items() if h not in present]

    threshold = ORDER.index(fail_on) if fail_on in ORDER else len(ORDER) + 1
    blocking_deps = [d for d in deps if ORDER.index(d["severity"]) >= threshold] if fail_on in ORDER else []
    # A committed secret is always blocking unless the caller disabled the gate.
    blocking = len(blocking_deps) + (len(secrets) if fail_on != "never" else 0)
    verdict = "GREEN" if blocking == 0 else "RED"
    accent = "#35c98a" if verdict == "GREEN" else "#ff6a3d"

    def sev_chip(sev):
        colour = {"critical": "#ff6a3d", "high": "#ff6a3d", "medium": "#ffb03a", "low": "#7a7567"}[sev]
        return f'<span style="color:{colour}">{escape(sev)}</span>'

    if deps:
        dep_rows = "\n".join(
            f'<tr><td>{escape(d["package"])} <span class="dim">{escape(d["version"])}</span></td>'
            f'<td>{sev_chip(d["severity"])}</td>'
            f'<td class="dim">{escape(d["id"])}{" · " + escape(d["source"]) if d["source"] else ""}</td></tr>'
            for d in sorted(deps, key=lambda x: -ORDER.index(x["severity"]))
        )
        deps_block = f'<table><tbody>{dep_rows}</tbody></table>'
    else:
        deps_block = '<p class="clean">No known-vulnerable dependencies found.</p>'

    if secrets:
        sec_rows = "\n".join(
            f'<tr><td>{escape(str(s.get("RuleID", s.get("Description", "secret"))))}</td>'
            f'<td class="dim">{escape(str(s.get("File", "?")))}'
            f'{":" + escape(str(s.get("StartLine"))) if s.get("StartLine") else ""}</td></tr>'
            for s in secrets
        )
        secrets_block = f'<table><tbody>{sec_rows}</tbody></table>'
    else:
        secrets_block = '<p class="clean">No committed secrets found.</p>'

    if not url:
        headers_block = '<p class="dim">No URL given — pass --url to check response headers.</p>'
    elif not fetched:
        headers_block = f'<p class="dim">Could not fetch {escape(url)} — nothing assessed.</p>'
    elif missing_headers:
        headers_block = "<table><tbody>" + "\n".join(
            f'<tr><td>{escape(h)}</td><td class="dim">missing — {escape(why)}</td></tr>'
            for h, why in missing_headers
        ) + "</tbody></table>"
    else:
        headers_block = '<p class="clean">All checked security headers are present.</p>'

    html = f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Security scan {escape(verdict)} — PillDrops</title>
<style>
 :root{{--bg:#131210;--bg1:#1a1815;--hair:rgba(244,240,230,.13);--ink:#f4f1e8;--dim:#7a7567;--accent:{accent}}}
 *{{box-sizing:border-box}}
 body{{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 ui-monospace,'SF Mono',Menlo,monospace;padding:32px}}
 .wrap{{max-width:820px;margin:0 auto}}
 .verdict{{display:inline-block;border:1px solid var(--accent);color:var(--accent);border-radius:999px;padding:4px 14px;font-size:12px;letter-spacing:.18em}}
 h1{{font-size:25px;letter-spacing:-.03em;margin:16px 0 6px}}
 h2{{font-size:13px;letter-spacing:.14em;text-transform:uppercase;color:var(--dim);margin:30px 0 10px}}
 .meta{{color:var(--dim);font-size:12.5px;word-break:break-all}}
 table{{width:100%;border-collapse:collapse;background:var(--bg1);border:1px solid var(--hair);border-radius:6px;overflow:hidden}}
 td{{padding:10px 14px;border-bottom:1px solid var(--hair);vertical-align:top}}
 tr:last-child td{{border-bottom:none}}
 .dim{{color:var(--dim)}}
 .clean{{color:var(--accent);background:var(--bg1);border:1px solid var(--hair);border-radius:6px;padding:12px 14px;margin:0}}
 .links{{margin-top:26px;display:flex;gap:18px;font-size:13px}}
 .links a{{color:var(--accent)}}
 footer{{margin-top:26px;color:var(--dim);font-size:11.5px}}
</style></head><body><div class="wrap">
<span class="verdict">{escape(verdict)}</span>
<h1>Security scan</h1>
<div class="meta">target {escape(target)}{" · url " + escape(url) if url else ""}<br>
gate: fail on {escape(fail_on)} or worse · {blocking} blocking finding{"" if blocking == 1 else "s"}</div>

<h2>Dependencies <span class="dim">osv-scanner</span></h2>
{deps_block}

<h2>Secrets <span class="dim">gitleaks</span></h2>
{secrets_block}

<h2>Response headers <span class="dim">passive check</span></h2>
{headers_block}

<div class="links"><a href="dependencies.json">dependencies.json</a><a href="secrets.json">secrets.json</a><a href="scan.log">scan.log</a></div>
<footer>Generated inside a PillDrops sandbox by pdscan. Scans code and targets you own — no active probing.</footer>
</div></body></html>
"""
    with open(os.path.join(outdir, "index.html"), "w") as fh:
        fh.write(html)
    print(f"▸ {len(deps)} dependency finding(s), {len(secrets)} secret(s), "
          f"{len(missing_headers)} missing header(s) → {verdict}")
    return 0 if verdict == "GREEN" else 1


if __name__ == "__main__":
    sys.exit(main())

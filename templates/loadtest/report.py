#!/usr/bin/env python3
"""Render a k6 --summary-export JSON into the load-test report page.

Usage: pd-report <summary.json> <out.html> <target> <verdict> <vus> <duration>

Only metrics k6 actually emitted are rendered — a missing metric is shown as
"—", never as a zero, so the report can't imply a measurement that didn't
happen.
"""
import json
import sys
from html import escape


def metric(metrics, name, key, scale=1.0):
    m = metrics.get(name)
    if not isinstance(m, dict):
        return None
    v = m.get(key)
    if v is None:
        return None
    try:
        return float(v) * scale
    except (TypeError, ValueError):
        return None


def fmt(v, unit="", digits=0):
    if v is None:
        return "—"
    if unit == "ms":
        return f"{v:,.0f} ms"
    if unit == "%":
        return f"{v:.2f}%"
    if unit == "rps":
        return f"{v:,.1f} /s"
    return f"{v:,.{digits}f}"


def main():
    if len(sys.argv) < 5:
        print("usage: pd-report <summary.json> <out.html> <target> <verdict> [vus] [duration]", file=sys.stderr)
        return 2
    src, dst, target, verdict = sys.argv[1:5]
    vus = sys.argv[5] if len(sys.argv) > 5 else "?"
    duration = sys.argv[6] if len(sys.argv) > 6 else "?"

    try:
        with open(src) as fh:
            data = json.load(fh)
    except (OSError, ValueError) as exc:
        print(f"pd-report: cannot read {src}: {exc}", file=sys.stderr)
        data = {}
    metrics = data.get("metrics", {}) if isinstance(data, dict) else {}

    rows = [
        ("Requests", fmt(metric(metrics, "http_reqs", "count"), digits=0)),
        ("Throughput", fmt(metric(metrics, "http_reqs", "rate"), "rps")),
        ("Failed requests", fmt(
            metric(metrics, "http_req_failed", "value", 100.0)
            if metric(metrics, "http_req_failed", "value") is not None
            else metric(metrics, "http_req_failed", "rate", 100.0), "%")),
        ("Latency avg", fmt(metric(metrics, "http_req_duration", "avg"), "ms")),
        ("Latency p95", fmt(metric(metrics, "http_req_duration", "p(95)"), "ms")),
        ("Latency max", fmt(metric(metrics, "http_req_duration", "max"), "ms")),
        ("Waiting (TTFB) avg", fmt(metric(metrics, "http_req_waiting", "avg"), "ms")),
        ("Iterations", fmt(metric(metrics, "iterations", "count"), digits=0)),
        ("Peak VUs", fmt(metric(metrics, "vus_max", "value"), digits=0)),
    ]

    ok = verdict == "GREEN"
    accent = "#35c98a" if ok else "#ff6a3d"
    body = "\n".join(
        f'<tr><td>{escape(k)}</td><td class="v">{escape(v)}</td></tr>' for k, v in rows
    )
    html = f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Load test {escape(verdict)} — PillDrops</title>
<style>
 :root{{--bg:#131210;--bg1:#1a1815;--hair:rgba(244,240,230,.13);--ink:#f4f1e8;--dim:#7a7567;--sub:#a8a294;--accent:{accent}}}
 *{{box-sizing:border-box}}
 body{{margin:0;background:var(--bg);color:var(--ink);font:14px/1.6 ui-monospace,'SF Mono',Menlo,monospace;padding:32px}}
 .wrap{{max-width:760px;margin:0 auto}}
 .verdict{{display:inline-block;border:1px solid var(--accent);color:var(--accent);border-radius:999px;padding:4px 14px;font-size:12px;letter-spacing:.18em}}
 h1{{font-size:26px;letter-spacing:-.03em;margin:16px 0 6px}}
 .meta{{color:var(--dim);font-size:12.5px;margin-bottom:26px;word-break:break-all}}
 table{{width:100%;border-collapse:collapse;background:var(--bg1);border:1px solid var(--hair);border-radius:6px;overflow:hidden}}
 td{{padding:11px 16px;border-bottom:1px solid var(--hair)}}
 tr:last-child td{{border-bottom:none}}
 td.v{{text-align:right;font-variant-numeric:tabular-nums;color:var(--accent)}}
 .links{{margin-top:22px;display:flex;gap:18px;font-size:13px}}
 .links a{{color:var(--accent)}}
 footer{{margin-top:28px;color:var(--dim);font-size:11.5px}}
</style></head><body><div class="wrap">
<span class="verdict">{escape(verdict)}</span>
<h1>Load test {"passed" if ok else "failed thresholds"}</h1>
<div class="meta">target {escape(target)} · {escape(str(vus))} VUs · {escape(str(duration))} hold<br>
thresholds: failures &lt; 1% · p95 &lt; 2000 ms</div>
<table><tbody>
{body}
</tbody></table>
<div class="links"><a href="run.log">run.log</a><a href="summary.json">summary.json</a><a href="script.js">script.js</a></div>
<footer>Generated inside a PillDrops sandbox by pdload. Metrics k6 did not emit are shown as “—”.</footer>
</div></body></html>
"""
    with open(dst, "w") as fh:
        fh.write(html)
    print(f"▸ report written: {dst}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

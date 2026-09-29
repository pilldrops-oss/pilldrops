#!/usr/bin/env python3
"""Generate standalone per-guide pages from the /guides hub.

Reads web/public/guides.html (source of truth for guide content), writes
web/public/guides/<slug>.html — one indexable page per guide with its own
title/description/canonical/OG + TechArticle/Breadcrumb JSON-LD — and rewrites
the hub's sections into summary cards linking to the standalone pages (so hub
and pages don't compete as duplicate content).

Run from repo root:  python3 web/scripts/gen_guide_pages.py
Then:                make web
"""
import html as htmlmod
import json
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
PUB = ROOT / "web" / "public"
OUT = PUB / "guides"
HUB = PUB / "guides.html"

# slug → (sidebar label, deploy-template id or None)
GUIDES = {
    "code-interpreter":   ("AI code interpreter",          "code-interpreter-v1"),
    "coding-agents":      ("Coding agents (Claude Code…)", "claude"),
    "browser-automation": ("Browser automation",           "playwright"),
    "computer-use":       ("Computer use / desktop",       "desktop"),
    "long-running":       ("Long-running agents",          "python-3.11"),
    "evals":              ("Evals & RL at scale",          "python-3.11"),
    "app-preview":        ("Full-stack app previews",      "nextjs"),
    "functions":          ("Agent tools as functions",     "python-3.11"),
    "untrusted-code":     ("Untrusted user code",          "python-3.11"),
    "ci-tests":           ("CI for AI-written code",       "python-3.11"),
    "data-pipeline":      ("Data jobs & ETL",              "python-3.11"),
    "custom-images":      ("Custom images",                None),
    "mobile-testing":     ("Mobile E2E (Android)",         "android"),
}
WORKLOADS = ["code-interpreter", "coding-agents", "browser-automation", "computer-use", "long-running", "evals"]
INFRA = ["app-preview", "functions", "untrusted-code", "ci-tests", "data-pipeline", "custom-images", "mobile-testing"]
ORDER = WORKLOADS + INFRA

hub = HUB.read_text(encoding="utf-8")

# ── extract every guide section verbatim ─────────────────────────────────────
sections = dict(re.findall(
    r'<section class="guide" id="guide-([a-z-]+)">(.*?)</section>', hub, re.S))
assert set(sections) == set(GUIDES), f"section/GUIDES mismatch: {set(sections) ^ set(GUIDES)}"

def text_of(fragment: str) -> str:
    return re.sub(r"\s+", " ", re.sub(r"<[^>]+>", "", fragment)).strip()

def meta(slug: str):
    body = sections[slug]
    h2 = re.search(r"<h2[^>]*>(.*?)</h2>", body, re.S).group(1)
    gwhat = re.search(r'<p class="gwhat">(.*?)</p>', body, re.S).group(1)
    title = htmlmod.unescape(text_of(h2))
    desc = htmlmod.unescape(text_of(gwhat))
    if len(desc) > 158:
        desc = desc[:155].rsplit(" ", 1)[0] + "…"
    return title, desc

def esc(s: str) -> str:
    return htmlmod.escape(s, quote=True)

def sidebar(current: str) -> str:
    def links(slugs):
        out = []
        for s in slugs:
            cur = ' class="cur" aria-current="page"' if s == current else ""
            out.append(f'    <a href="/guides/{s}"{cur}>{GUIDES[s][0]}</a>')
        return "\n".join(out)
    return (f'  <aside class="side">\n'
            f'    <a href="/guides" class="allg">← All guides</a>\n'
            f'    <h4>Agent workloads</h4>\n{links(WORKLOADS)}\n'
            f'    <h4>Apps &amp; infra</h4>\n{links(INFRA)}\n'
            f'  </aside>')

# head chunk (everything before <nav) and nav (reused verbatim per page)
head_end = hub.index('<nav class="nav">')
head = hub[:head_end]
nav = hub[head_end:hub.index("</nav>") + len("</nav>")]

EXTRA_CSS = """<style>
/* standalone guide page */
.guide{border-top:none;margin-top:0;padding-top:6px}
.guide h1{font-size:clamp(24px,2.6vw,31px);margin:0 0 14px;letter-spacing:-.01em;line-height:1.25}
.side .allg{color:var(--phos);font-size:13px;margin-bottom:4px}
.side a.cur{color:var(--phos);border-left-color:var(--phos);background:var(--coal1)}
.related{margin-top:56px;border-top:1px solid var(--hair);padding-top:28px;display:flex;gap:12px 22px;flex-wrap:wrap;align-items:center;font-size:14px}
.related a{color:var(--phos)}
.related .ghost{color:var(--sub)}
</style>
"""

for slug in ORDER:
    title, desc = meta(slug)
    label, tpl = GUIDES[slug]
    url = f"https://pilldrops.com/guides/{slug}"

    h = head
    h = re.sub(r"<title>.*?</title>", f"<title>{esc(title)} — PillDrops Guides</title>", h, flags=re.S)
    h = re.sub(r'<meta name="description" content="[^"]*">',
               f'<meta name="description" content="{esc(desc)} Step-by-step guide with SDK and CLI code.">', h)
    h = h.replace('<link rel="canonical" href="https://pilldrops.com/guides">',
                  f'<link rel="canonical" href="{url}">')
    h = h.replace('<meta property="og:url" content="https://pilldrops.com/guides">',
                  f'<meta property="og:url" content="{url}">')
    h = re.sub(r'<meta property="og:title" content="[^"]*">',
               f'<meta property="og:title" content="{esc(title)}">', h)
    h = re.sub(r'<meta property="og:description" content="[^"]*">',
               f'<meta property="og:description" content="{esc(desc)}">', h)
    h = h.replace('<meta property="og:type" content="website">', '<meta property="og:type" content="article">')
    h = re.sub(r'<meta name="twitter:title" content="[^"]*">',
               f'<meta name="twitter:title" content="{esc(title)}">', h)
    h = re.sub(r'<meta name="twitter:description" content="[^"]*">',
               f'<meta name="twitter:description" content="{esc(desc)}">', h)
    h = h.replace("</style>\n", "</style>\n" + EXTRA_CSS, 1)

    body = sections[slug]
    body = re.sub(r"<h2([^>]*)>", r"<h1\1>", body, count=1)
    body = body.replace("</h2>", "</h1>", 1)

    nav_page = nav.replace('<span class="crumb">/ GUIDES</span>',
                           f'<span class="crumb"><a href="/guides">/ GUIDES</a> / {slug.upper().replace("-", " ")}</span>')

    i = ORDER.index(slug)
    prev_s, next_s = ORDER[i - 1], ORDER[(i + 1) % len(ORDER)]
    related = ['<div class="related">']
    if tpl:
        related.append(f'    <a href="/dashboard#/deploy?template={tpl}"><b>Launch the {tpl} template →</b></a>')
    related.append('    <a class="ghost" href="/docs">API reference</a>')
    related.append(f'    <a class="ghost" href="/guides/{prev_s}">← {GUIDES[prev_s][0]}</a>')
    related.append(f'    <a class="ghost" href="/guides/{next_s}">{GUIDES[next_s][0]} →</a>')
    related.append("</div>")

    jtitle = json.dumps(htmlmod.unescape(title))
    jdesc = json.dumps(htmlmod.unescape(desc))
    jsonld = f"""<script type="application/ld+json">
{{
  "@context": "https://schema.org",
  "@type": "TechArticle",
  "headline": {jtitle},
  "description": {jdesc},
  "url": "{url}",
  "image": "https://pilldrops.com/og.png",
  "author": {{"@type": "Organization", "name": "PillDrops", "url": "https://pilldrops.com/"}},
  "publisher": {{"@type": "Organization", "name": "PillDrops", "url": "https://pilldrops.com/"}}
}}
</script>
<script type="application/ld+json">
{{
  "@context": "https://schema.org",
  "@type": "BreadcrumbList",
  "itemListElement": [
    {{"@type":"ListItem","position":1,"name":"PillDrops","item":"https://pilldrops.com/"}},
    {{"@type":"ListItem","position":2,"name":"Guides","item":"https://pilldrops.com/guides"}},
    {{"@type":"ListItem","position":3,"name":{jtitle},"item":"{url}"}}
  ]
}}
</script>"""

    page = f"""{h}{nav_page}

<div class="layout">
{sidebar(slug)}

  <main class="doc">
    <section class="guide" id="guide-{slug}">{body}</section>
{chr(10).join(related)}
    <div style="margin-top:44px;display:flex;gap:14px;align-items:center;flex-wrap:wrap">
      <a class="btn" href="/dashboard">Start free — $5 credits</a>
      <a href="/guides" style="color:var(--phos);font-size:14px">Browse all 13 guides →</a>
    </div>
  </main>
</div>

{jsonld}

<script>
document.getElementById('mode-t').addEventListener('click', function(){{
  var dark = document.documentElement.classList.toggle('dark');
  try{{ localStorage.setItem('sf.theme', dark ? 'dark' : 'light'); }}catch(e){{}}
  var m = document.querySelector('meta[name="theme-color"]');
  if (m) m.setAttribute('content', dark ? '#131210' : '#f7f5f0');
}});
</script>
</body>
</html>
"""
    OUT.mkdir(exist_ok=True)
    (OUT / f"{slug}.html").write_text(page, encoding="utf-8")
    print(f"guides/{slug}.html  ({title[:50]})")

# ── rewrite the hub: full sections → summary cards linking to the pages ──────
for slug in ORDER:
    title, desc_full = meta(slug)
    body = sections[slug]
    gkick = re.search(r'<p class="gkick">.*?</p>', body, re.S).group(0)
    gwhat = re.search(r'<p class="gwhat">.*?</p>', body, re.S).group(0)
    card = (f'<section class="guide" id="guide-{slug}">\n'
            f'      {gkick}\n'
            f'      <h2 class="disp"><a href="/guides/{slug}">{title}</a></h2>\n'
            f'      {gwhat}\n'
            f'      <p style="margin:0"><a href="/guides/{slug}" style="color:var(--phos);font-weight:600">Read the full guide →</a></p>\n'
            f'    </section>')
    hub = re.sub(r'<section class="guide" id="guide-' + slug + r'">.*?</section>',
                 lambda _m: card, hub, count=1, flags=re.S)

# hub sidebar → page links; ItemList rebuilt wholesale with URLs
for slug in ORDER:
    hub = hub.replace(f'href="#guide-{slug}"', f'href="/guides/{slug}"')
items = ",\n    ".join(
    json.dumps({"@type": "ListItem", "position": i + 1, "name": meta(s)[0],
                "url": f"https://pilldrops.com/guides/{s}"}, ensure_ascii=False)
    for i, s in enumerate(ORDER))
hub = re.sub(r'"itemListElement": \[.*?\]', '"itemListElement": [\n    ' + items + "\n  ]", hub, flags=re.S)

HUB.write_text(hub, encoding="utf-8")
print("hub rewritten: sections → summary cards, sidebar + ItemList link pages")

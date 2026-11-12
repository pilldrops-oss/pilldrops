// Lightweight browser bridge: headless Chromium (CDP) → JPEG screencast over a
// WebSocket to a canvas-less <img> viewer, with mouse/key/scroll/nav sent back
// as CDP Input events. No X server, no VNC, no desktop — just Chromium + this.
// The viewer is skinned to look like Chrome (tab strip + omnibox + nav) and the
// render resolution is matched to the client's actual pixel size so it stays
// sharp instead of upscaling a small frame. Only dependency: ws.
const http = require('http')
const net = require('net')
const WebSocket = require('ws')

const PORT = process.env.PORT || 8000
const START_URL = process.env.START_URL || 'https://example.com'
const QUALITY = 72          // JPEG quality (2x frames carry the sharpness now)
const MAXFW = 3840, MAXFH = 2160 // frame ceiling (device px) — allows 2x retina up to ~1920 CSS wide
const CDP_PORT = 9222       // Chromium's remote-debugging port (localhost-only inside the VM)
// Per-sandbox token gating the CDP endpoints (/cdp, /json, /devtools,
// /session) — callers present it as ?token= or X-Pd-Token. Self-minted at boot
// when not supplied, and written to /tmp/pd_cdp_token so the control plane can
// read it back and hand the caller a ready cdp_url. The human viewer (/ and
// /ws) stays open. Playwright: connectOverCDP('wss://…/cdp?token=<t>').
// Access is gated by the portal preview proxy (private-by-default previews),
// so the bridge itself stays open — no per-app token. PD_CDP_TOKEN can still
// force a bridge-level gate for standalone use.
const CDP_TOKEN = process.env.PD_CDP_TOKEN || ''

// Shown when the viewer is opened without the per-session token.
const UNAUTH_PAGE = `<!doctype html><meta charset="utf-8"><title>Private browser session</title>
<style>html,body{margin:0;height:100%;display:grid;place-items:center;background:#0e0e10;color:#e8eaed;font-family:system-ui,Segoe UI,Roboto,sans-serif}
.c{max-width:440px;text-align:center;padding:32px}h1{font-size:19px;margin:0 0 10px}p{color:#9aa0a6;font-size:14px;line-height:1.5;margin:0}code{color:#8ab4f8}</style>
<div class="c"><h1>🔒 This browser session is private</h1>
<p>Open it with the <code>viewer_url</code> from the sandbox's create response — it carries the access token. The bare URL isn't accessible.</p></div>`

const VIEWER = `<!doctype html><html><head><meta charset="utf-8"><title>Browser</title>
<style>
  :root{--strip:#dee1e6;--tool:#fff;--omni:#f1f3f4;--ico:#5f6368;--txt:#202124}
  @media (prefers-color-scheme:dark){:root{--strip:#202124;--tool:#292a2d;--omni:#3c4043;--ico:#c4c7c5;--txt:#e8eaed}}
  html,body{margin:0;height:100%;overflow:hidden;background:var(--strip);font-family:'Segoe UI',system-ui,Roboto,Arial,sans-serif;-webkit-font-smoothing:antialiased}
  #tabs{height:36px;background:var(--strip);display:flex;align-items:flex-end;padding:0 8px;gap:2px;user-select:none}
  .tab{display:flex;align-items:center;gap:9px;background:var(--tool);height:30px;border-radius:10px 10px 0 0;padding:0 12px;min-width:150px;max-width:250px;font-size:12.5px;color:var(--txt)}
  .tab img{width:16px;height:16px;border-radius:3px;flex:none}
  .tab .ttl{overflow:hidden;white-space:nowrap;text-overflow:ellipsis;flex:1}
  .tab .x{color:var(--ico);border-radius:50%;width:18px;height:18px;display:flex;align-items:center;justify-content:center;font-size:15px;flex:none}
  #newtab{color:var(--ico);width:28px;height:28px;display:flex;align-items:center;justify-content:center;border-radius:50%;font-size:20px;margin-bottom:1px}
  #bar{height:48px;background:var(--tool);display:flex;align-items:center;gap:2px;padding:0 10px}
  .nav{width:36px;height:36px;border-radius:50%;border:0;background:transparent;color:var(--ico);cursor:pointer;display:flex;align-items:center;justify-content:center}
  .nav:hover{background:rgba(128,128,128,.15)}
  .nav:disabled{opacity:.35;cursor:default}
  #omni{flex:1;display:flex;align-items:center;height:38px;background:var(--omni);border-radius:19px;padding:0 15px;gap:11px;margin:0 8px}
  #omni:focus-within{background:var(--tool);box-shadow:0 1px 6px rgba(32,33,36,.28)}
  #lock{color:var(--ico);display:flex;flex:none}
  #url{flex:1;border:0;background:transparent;outline:0;font-size:14px;color:var(--txt)}
  #dot{width:8px;height:8px;border-radius:50%;background:#bbb;flex:none}
  #screen{display:block;width:100vw;height:calc(100vh - 84px);object-fit:fill;background:#fff;cursor:default}
</style></head><body>
<div id="tabs">
  <div class="tab"><img id="fav" alt=""><span class="ttl" id="ttl">New Tab</span><span class="x">&times;</span></div>
  <div id="newtab">+</div>
</div>
<div id="bar">
  <button class="nav" id="back" title="Back"><svg viewBox="0 0 24 24" width="20" height="20"><path fill="currentColor" d="M15.41 7.41 14 6l-6 6 6 6 1.41-1.41L10.83 12z"/></svg></button>
  <button class="nav" id="fwd" title="Forward"><svg viewBox="0 0 24 24" width="20" height="20"><path fill="currentColor" d="M8.59 16.59 10 18l6-6-6-6-1.41 1.41L13.17 12z"/></svg></button>
  <button class="nav" id="reload" title="Reload"><svg viewBox="0 0 24 24" width="20" height="20"><path fill="currentColor" d="M17.65 6.35A7.96 7.96 0 0 0 12 4a8 8 0 1 0 8 8h-2a6 6 0 1 1-6-6c1.66 0 3.14.69 4.22 1.78L13 11h7V4z"/></svg></button>
  <div id="omni">
    <span id="lock" title="Connection is secure"><svg viewBox="0 0 24 24" width="14" height="14"><path fill="currentColor" d="M12 17a2 2 0 0 0 2-2 2 2 0 0 0-2-2 2 2 0 0 0-2 2 2 2 0 0 0 2 2m6-9a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2h1V6a5 5 0 0 1 5-5 5 5 0 0 1 5 5v2h1m-6-5a3 3 0 0 0-3 3v2h6V6a3 3 0 0 0-3-3z"/></svg></span>
    <input id="url" placeholder="Search or type a URL" spellcheck="false" autocomplete="off">
    <span id="dot" title="stream status"></span>
  </div>
  <button class="nav" id="menu" title="Menu"><svg viewBox="0 0 24 24" width="20" height="20"><path fill="currentColor" d="M12 8a2 2 0 0 0 2-2 2 2 0 0 0-2-2 2 2 0 0 0-2 2 2 2 0 0 0 2 2m0 2a2 2 0 0 0-2 2 2 2 0 0 0 2 2 2 2 0 0 0 2-2 2 2 0 0 0-2-2m0 6a2 2 0 0 0-2 2 2 2 0 0 0 2 2 2 2 0 0 0 2-2 2 2 0 0 0-2-2z"/></svg></button>
</div>
<img id="screen" tabindex="0" draggable="false">
<script>
const img=document.getElementById('screen'),url=document.getElementById('url'),dot=document.getElementById('dot');
const ttl=document.getElementById('ttl'),fav=document.getElementById('fav');
let view={w:1280,h:800}, ws, urlFocused=false;
const base=location.pathname.replace(/\\/+$/,'');
function sendSize(){
  if(!ws||ws.readyState!==1)return;
  const r=img.getBoundingClientRect();
  const dpr=Math.min(window.devicePixelRatio||1,2);
  // Send the CSS size of the display box (the remote page lays out at exactly
  // this size, so it fits perfectly) plus dpr (used only for render sharpness).
  ws.send(JSON.stringify({t:'size',w:Math.round(r.width),h:Math.round(r.height),dpr:dpr}));
}
function setFav(u){try{const h=new URL(u).host;fav.src='https://www.google.com/s2/favicons?domain='+h+'&sz=32';}catch(e){fav.removeAttribute('src');}}
function connect(){
  ws=new WebSocket((location.protocol==='https:'?'wss':'ws')+'://'+location.host+base+'/ws'+(location.search||''));
  ws.onopen=()=>{dot.style.background='#34a853';queueSize();setTimeout(queueSize,120);setTimeout(queueSize,500);};
  ws.onclose=()=>{dot.style.background='#ea4335';setTimeout(connect,1000);};
  ws.onmessage=e=>{const m=JSON.parse(e.data);
    if(m.t==='frame')img.src='data:image/jpeg;base64,'+m.data;
    else if(m.t==='view')view={w:m.w,h:m.h};
    else if(m.t==='url'){if(!urlFocused)url.value=m.url;setFav(m.url);}
    else if(m.t==='title')ttl.textContent=m.title||'New Tab';
    else if(m.t==='sel'){if(m.text)try{navigator.clipboard.writeText(m.text);}catch(e){}}
  };
}
connect();
// Track the real box size continuously so a premature first measurement (layout
// not settled at ws.open) always self-corrects, and window resizes re-fit.
let rt;function queueSize(){clearTimeout(rt);rt=setTimeout(sendSize,140);}
window.addEventListener('resize',queueSize);
if(window.ResizeObserver)new ResizeObserver(queueSize).observe(img);
window.addEventListener('load',queueSize);
function P(ev){const r=img.getBoundingClientRect();return{x:Math.round((ev.clientX-r.left)/r.width*view.w),y:Math.round((ev.clientY-r.top)/r.height*view.h)};}
function S(o){if(ws&&ws.readyState===1)ws.send(JSON.stringify(o));}
function BT(b){return b===2?'right':b===1?'middle':'left';}
function KM(ev){return (ev.altKey?1:0)|(ev.ctrlKey?2:0)|(ev.metaKey?4:0)|(ev.shiftKey?8:0);}
let lx=0,ly=0,lastMove=0;
img.addEventListener('mousedown',ev=>{ev.preventDefault();const p=P(ev);lx=p.x;ly=p.y;S({t:'down',button:BT(ev.button),cc:ev.detail||1,...p});img.focus();});
img.addEventListener('mouseup',ev=>{ev.preventDefault();const p=P(ev);S({t:'up',button:BT(ev.button),cc:ev.detail||1,...p});});
img.addEventListener('mousemove',ev=>{const now=Date.now();if(!ev.buttons&&now-lastMove<40)return;lastMove=now;const p=P(ev);lx=p.x;ly=p.y;S({t:'move',b:ev.buttons,...p});});
img.addEventListener('contextmenu',ev=>ev.preventDefault());
img.addEventListener('wheel',ev=>{ev.preventDefault();S({t:'scroll',x:lx,y:ly,dx:ev.deltaX,dy:ev.deltaY});},{passive:false});
// Clipboard: Ctrl/Cmd+C reads the remote selection into the local clipboard;
// Ctrl/Cmd+V pastes the local clipboard into the page. Everything else is a key.
async function doCopy(){S({t:'getsel'});}
async function doPaste(){try{const t=await navigator.clipboard.readText();if(t)S({t:'paste',text:t});}catch(e){}}
img.addEventListener('keydown',ev=>{ev.preventDefault();
  const mod=ev.ctrlKey||ev.metaKey, k=ev.key.toLowerCase();
  if(mod&&k==='c'){doCopy();return;}
  if(mod&&k==='v'){doPaste();return;}
  S({t:'kd',key:ev.key,code:ev.code,kc:ev.keyCode,m:KM(ev)});
});
img.addEventListener('keyup',ev=>{ev.preventDefault();S({t:'ku',key:ev.key,code:ev.code,kc:ev.keyCode,m:KM(ev)});});
url.addEventListener('focus',()=>{urlFocused=true;url.select();});
url.addEventListener('blur',()=>{urlFocused=false;});
url.addEventListener('keydown',ev=>{if(ev.key==='Enter'){S({t:'nav',url:url.value});img.focus();}});
document.getElementById('back').onclick=()=>S({t:'back'});
document.getElementById('fwd').onclick=()=>S({t:'forward'});
document.getElementById('reload').onclick=()=>S({t:'reload'});
</script></body></html>`

async function pageWsUrl() {
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch('http://127.0.0.1:9222/json')
      const list = await r.json()
      const pg = list.find(t => t.type === 'page')
      if (pg && pg.webSocketDebuggerUrl) return pg.webSocketDebuggerUrl
    } catch (e) { /* not up yet */ }
    await new Promise(s => setTimeout(s, 1000))
  }
  throw new Error('Chromium CDP page not found on :9222')
}

;(async () => {
  const wsUrl = await pageWsUrl()
  const cdp = new WebSocket(wsUrl)
  let id = 0
  const pending = new Map()
  // cmd() correlates CDP responses to callbacks so we can read navigation
  // history / page title without a heavyweight client library.
  const cmd = (method, params, cb) => {
    const i = ++id
    if (cb) pending.set(i, cb)
    try { cdp.send(JSON.stringify({ id: i, method, params: params || {} })) } catch (e) {}
    return i
  }
  const clients = new Set()
  const broadcast = (o) => { const s = JSON.stringify(o); for (const c of clients) if (c.readyState === 1) c.send(s) }
  let view = { w: 1280, h: 800 }
  // The page's address and title, replayed to a viewer that connects after the
  // navigation that announced them.
  let pageUrl = '', pageTitle = ''

  // Lay the remote page out at the client's CSS box size so it FITS exactly (no
  // overflow), and use deviceScaleFactor purely for sharpness — bounded so the
  // streamed frame never exceeds MAXFW×MAXFH device px.
  const applySize = (w, h, dpr) => {
    w = Math.max(400, Math.min(Math.round(w), 3000))
    h = Math.max(300, Math.min(Math.round(h), 2000))
    // Integer deviceScaleFactor only — a fractional one makes some sites lay out
    // incorrectly. Use 2 (retina-sharp) when the 2x frame still fits the caps,
    // else 1. The page always LAYS OUT at w×h, so it fits regardless of scale.
    const scale = (dpr >= 2 && w * 2 <= MAXFW && h * 2 <= MAXFH) ? 2 : 1
    view = { w, h } // logical CSS px — the coordinate space for input events
    cmd('Emulation.setDeviceMetricsOverride', { width: w, height: h, deviceScaleFactor: scale, mobile: false })
    cmd('Page.stopScreencast')
    cmd('Page.startScreencast', { format: 'jpeg', quality: QUALITY, maxWidth: w * scale, maxHeight: h * scale, everyNthFrame: 1 })
    broadcast({ t: 'view', w, h })
  }

  // ── Session capture (testing evidence): a bounded ring buffer of network
  // requests (exported as HAR 1.2) and console messages, so a test/agent run
  // leaves a downloadable record of what happened. Pure CDP, capped to bound
  // memory. Exposed token-gated at /session/har and /session/console.
  const CAP = 2000
  const inflight = new Map()   // requestId → partial entry
  const harEntries = []        // finished entries (ring, capped)
  const consoleLog = []        // {ts, level, text} (ring, capped)
  const pushCap = (arr, v) => { arr.push(v); if (arr.length > CAP) arr.shift() }
  const hdrArr = (h) => Object.keys(h || {}).map(k => ({ name: k, value: String(h[k]) }))

  cdp.on('open', () => {
    cmd('Page.enable')
    cmd('Runtime.enable')
    cmd('Network.enable')
    cmd('Log.enable')
    cmd('Page.navigate', { url: START_URL })
    cmd('Page.startScreencast', { format: 'jpeg', quality: QUALITY, maxWidth: view.w, maxHeight: view.h, everyNthFrame: 1 })
  })
  cdp.on('message', buf => {
    let m; try { m = JSON.parse(buf) } catch { return }
    if (m.id && pending.has(m.id)) { const cb = pending.get(m.id); pending.delete(m.id); try { cb(m.result) } catch (e) {}; return }
    if (m.method === 'Page.screencastFrame') {
      broadcast({ t: 'frame', data: m.params.data })
      cmd('Page.screencastFrameAck', { sessionId: m.params.sessionId })
    } else if (m.method === 'Page.frameNavigated' && m.params.frame && !m.params.frame.parentId) {
      pageUrl = m.params.frame.url
      broadcast({ t: 'url', url: pageUrl })
    } else if (m.method === 'Page.loadEventFired') {
      cmd('Runtime.evaluate', { expression: 'document.title', returnByValue: true }, r => {
        pageTitle = (r && r.result && r.result.value) || ''
        broadcast({ t: 'title', title: pageTitle })
      })
    } else if (m.method === 'Network.requestWillBeSent') {
      const p = m.params
      inflight.set(p.requestId, { startedDateTime: new Date(p.wallTime * 1000).toISOString(), t0: p.timestamp,
        request: { method: p.request.method, url: p.request.url, httpVersion: 'HTTP/1.1', cookies: [], queryString: [],
          headers: hdrArr(p.request.headers), headersSize: -1, bodySize: (p.request.postData || '').length || 0 },
        _type: p.type || '' })
    } else if (m.method === 'Network.responseReceived') {
      const e = inflight.get(m.params.requestId); if (e) { const r = m.params.response
        e._resp = { status: r.status, statusText: r.statusText || '', httpVersion: r.protocol || 'HTTP/1.1', cookies: [],
          headers: hdrArr(r.headers), content: { size: -1, mimeType: (r.mimeType || '') }, redirectURL: '', headersSize: -1, bodySize: -1 }
        e._t1 = m.params.timestamp }
    } else if (m.method === 'Network.loadingFinished' || m.method === 'Network.loadingFailed') {
      const id = m.params.requestId, e = inflight.get(id); if (e) {
        const time = Math.max(0, Math.round(((m.params.timestamp - e.t0) || 0) * 1000))
        const resp = e._resp || { status: 0, statusText: m.params.errorText || 'failed', httpVersion: 'HTTP/1.1', cookies: [], headers: [], content: { size: 0, mimeType: '' }, redirectURL: '', headersSize: -1, bodySize: -1 }
        if (m.params.encodedDataLength != null) resp.bodySize = m.params.encodedDataLength
        pushCap(harEntries, { startedDateTime: e.startedDateTime, time, request: e.request, response: resp,
          cache: {}, timings: { send: 0, wait: Math.max(0, time), receive: 0 }, _resourceType: e._type })
        inflight.delete(id) }
    } else if (m.method === 'Runtime.consoleAPICalled') {
      const p = m.params
      const text = (p.args || []).map(a => a.value !== undefined ? a.value : (a.description || a.unserializableValue || '')).join(' ')
      pushCap(consoleLog, { ts: new Date((p.timestamp) || Date.now()).toISOString(), level: p.type || 'log', text: String(text).slice(0, 4000) })
    } else if (m.method === 'Log.entryAdded') {
      const e = m.params.entry || {}
      pushCap(consoleLog, { ts: new Date((e.timestamp) || Date.now()).toISOString(), level: e.level || 'info', text: String(e.text || '').slice(0, 4000), url: e.url || '' })
    }
  })

  // Chrome-accurate back/forward via the real navigation history, so the arrows
  // walk history rather than trying (and failing) to run javascript:history.*.
  const historyGo = (delta) => cmd('Page.getNavigationHistory', {}, res => {
    if (!res || !res.entries) return
    const t = res.currentIndex + delta
    if (t >= 0 && t < res.entries.length) cmd('Page.navigateToHistoryEntry', { entryId: res.entries[t].id })
  })

  // Full-fidelity key dispatch: real keyDown/keyUp with code + virtual keycode +
  // modifier bitmask (Alt1/Ctrl2/Meta4/Shift8), and text for printable keys so
  // typing, shortcuts, Enter/Tab/Backspace/arrows all behave like a real browser.
  const dispatchKey = (type, e) => {
    const printable = e.key && e.key.length === 1 && !(e.m & 6) // no Ctrl/Meta → emits a character
    const p = { type, key: e.key, code: e.code, windowsVirtualKeyCode: e.kc, nativeVirtualKeyCode: e.kc, modifiers: e.m || 0 }
    if (type === 'keyDown') {
      if (e.key === 'Enter') p.text = '\r'
      else if (printable) p.text = e.key
    }
    cmd('Input.dispatchKeyEvent', p)
  }

  // ── CDP passthrough: makes this sandbox a drop-in target for Playwright /
  // Puppeteer / Stagehand / browser-use via chromium.connectOverCDP(). We serve
  // Chromium's own /json* discovery endpoints (rewriting the localhost debugger
  // URLs to this sandbox's public wss host) and proxy the /devtools/* WebSocket
  // upgrades straight through to 127.0.0.1:9222. Chromium is launched with
  // --remote-allow-origins=* so it accepts the proxied upgrades.
  const tokenOK = (req) => {
    if (!CDP_TOKEN) return true
    if ((req.headers['x-pd-token'] || '') === CDP_TOKEN) return true
    try { return new URL(req.url, 'http://x').searchParams.get('token') === CDP_TOKEN } catch { return false }
  }
  const publicWsBase = (req) => {
    const host = req.headers.host || ('127.0.0.1:' + PORT)
    const proto = req.headers['x-forwarded-proto'] === 'http' ? 'ws' : 'wss'
    return proto + '://' + host
  }
  const proxyCdpHttp = (req, res) => {
    const preq = http.request({ host: '127.0.0.1', port: CDP_PORT, path: req.url.split('?')[0], method: 'GET',
      headers: { host: '127.0.0.1:' + CDP_PORT } }, pres => {
      let body = ''
      pres.setEncoding('utf8')
      pres.on('data', d => body += d)
      pres.on('end', () => {
        // Point discovery at THIS host so the client dials back through the proxy.
        const base = publicWsBase(req)
        body = body.split('ws://127.0.0.1:' + CDP_PORT).join(base).split('ws://localhost:' + CDP_PORT).join(base)
        res.writeHead(pres.statusCode || 200, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*' })
        res.end(body)
      })
    })
    preq.on('error', () => { res.writeHead(502); res.end('{"error":"cdp unavailable"}') })
    preq.end()
  }

  const server = http.createServer((req, res) => {
    const path = (req.url || '').split('?')[0]
    if (path.startsWith('/json')) {
      if (!tokenOK(req)) { res.writeHead(401); res.end('{"error":"unauthorized"}'); return }
      return proxyCdpHttp(req, res)
    }
    // Auth persistence: GET exports all cookies, POST {cookies:[...]} restores
    // them into a fresh sandbox — so an agent can stay logged in across runs.
    if (path === '/session/cookies') {
      if (!tokenOK(req)) { res.writeHead(401, { 'Content-Type': 'application/json' }); res.end('{"error":"unauthorized"}'); return }
      const done = (o) => { res.writeHead(200, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*' }); res.end(JSON.stringify(o)) }
      if (req.method === 'GET') { cmd('Network.getAllCookies', {}, r => done({ cookies: (r && r.cookies) || [] })); return }
      if (req.method === 'POST') {
        let body = ''; req.on('data', d => body += d); req.on('end', () => {
          let cookies = []; try { const j = JSON.parse(body || '{}'); cookies = j.cookies || (Array.isArray(j) ? j : []) } catch (e) {}
          cmd('Network.setCookies', { cookies }, () => done({ ok: true, set: cookies.length }))
        }); return
      }
      res.writeHead(405); res.end('{"error":"method not allowed"}'); return
    }
    // Session capture (testing evidence). GET downloads; POST clears the buffer
    // (start a clean recording before a run).
    if (path === '/session/har' || path === '/session/console') {
      if (!tokenOK(req)) { res.writeHead(401, { 'Content-Type': 'application/json' }); res.end('{"error":"unauthorized"}'); return }
      const isHar = path === '/session/har'
      if (req.method === 'POST') { if (isHar) harEntries.length = 0; else consoleLog.length = 0; res.writeHead(200, { 'Content-Type': 'application/json' }); res.end('{"ok":true,"cleared":true}'); return }
      const payload = isHar
        ? { log: { version: '1.2', creator: { name: 'pilldrops-browser', version: '1.0' }, pages: [], entries: harEntries } }
        : { entries: consoleLog }
      res.writeHead(200, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': '*',
        'Content-Disposition': 'inline; filename="session.' + (isHar ? 'har' : 'console.json') + '"' })
      res.end(JSON.stringify(payload)); return
    }
    // The viewer is PRIVATE — it drives a browser that may hold logged-in
    // sessions, so it needs the same per-session token as the CDP endpoints.
    // Open the viewer_url from the create response (…/?token=<secret>).
    if (!tokenOK(req)) {
      res.writeHead(401, { 'Content-Type': 'text/html' })
      res.end(UNAUTH_PAGE)
      return
    }
    res.setHeader('Content-Type', 'text/html'); res.end(VIEWER)
  })

  // Pipe a WS upgrade straight to Chromium's debugger at the given CDP path,
  // forcing Host to localhost (past Chromium's DNS-rebinding guard).
  const pipeToCdp = (cdpPath, req, socket, head) => {
    const up = net.connect(CDP_PORT, '127.0.0.1', () => {
      const h = req.headers
      let head_ = 'GET ' + cdpPath + ' HTTP/1.1\r\nHost: 127.0.0.1:' + CDP_PORT + '\r\n' +
        'Upgrade: websocket\r\nConnection: Upgrade\r\n'
      if (h['sec-websocket-key']) head_ += 'Sec-WebSocket-Key: ' + h['sec-websocket-key'] + '\r\n'
      if (h['sec-websocket-version']) head_ += 'Sec-WebSocket-Version: ' + h['sec-websocket-version'] + '\r\n'
      if (h['sec-websocket-protocol']) head_ += 'Sec-WebSocket-Protocol: ' + h['sec-websocket-protocol'] + '\r\n'
      if (h['sec-websocket-extensions']) head_ += 'Sec-WebSocket-Extensions: ' + h['sec-websocket-extensions'] + '\r\n'
      head_ += '\r\n'
      up.write(head_)
      if (head && head.length) up.write(head)
      socket.pipe(up); up.pipe(socket)
    })
    up.on('error', () => socket.destroy())
    socket.on('error', () => up.destroy())
  }

  // Resolve (and cache) Chromium's browser-level debugger path, e.g.
  // /devtools/browser/<guid>. Lets /cdp be a STABLE, guid-free endpoint.
  let browserPath = null
  const browserWsPath = () => new Promise((resolve) => {
    if (browserPath) return resolve(browserPath)
    http.get({ host: '127.0.0.1', port: CDP_PORT, path: '/json/version', headers: { host: '127.0.0.1:' + CDP_PORT } }, r => {
      let b = ''; r.on('data', d => b += d); r.on('end', () => {
        try { const u = JSON.parse(b).webSocketDebuggerUrl || ''; const i = u.indexOf('/devtools/'); browserPath = i >= 0 ? u.slice(i) : null } catch { browserPath = null }
        resolve(browserPath)
      })
    }).on('error', () => resolve(null))
  })

  // Route WS upgrades: /ws = viewer stream; /cdp = stable Playwright endpoint
  // (connectOverCDP('wss://<sandbox>/cdp')); /devtools/* = raw CDP passthrough.
  const wss = new WebSocket.Server({ noServer: true })
  server.on('upgrade', (req, socket, head) => {
    const url = req.url || ''
    if (url === '/ws' || url.startsWith('/ws?')) {
      if (!tokenOK(req)) { socket.destroy(); return }
      wss.handleUpgrade(req, socket, head, ws => wss.emit('connection', ws, req))
    } else if (url === '/cdp' || url.startsWith('/cdp?')) {
      if (!tokenOK(req)) { socket.destroy(); return }
      browserWsPath().then(p => p ? pipeToCdp(p, req, socket, head) : socket.destroy()).catch(() => socket.destroy())
    } else if (url.startsWith('/devtools/')) {
      if (!tokenOK(req)) { socket.destroy(); return }
      pipeToCdp(url.split('?')[0], req, socket, head)
    } else {
      socket.destroy()
    }
  })
  wss.on('connection', ws => {
    clients.add(ws)
    ws.send(JSON.stringify({ t: 'view', w: view.w, h: view.h }))
    if (pageUrl) ws.send(JSON.stringify({ t: 'url', url: pageUrl }))
    if (pageTitle) ws.send(JSON.stringify({ t: 'title', title: pageTitle }))
    ws.on('close', () => clients.delete(ws))
    ws.on('message', raw => {
      let e; try { e = JSON.parse(raw) } catch { return }
      const bmask = { left: 1, right: 2, middle: 4 }
      if (e.t === 'down') cmd('Input.dispatchMouseEvent', { type: 'mousePressed', x: e.x, y: e.y, button: e.button || 'left', buttons: bmask[e.button] || 1, clickCount: e.cc || 1 })
      else if (e.t === 'up') cmd('Input.dispatchMouseEvent', { type: 'mouseReleased', x: e.x, y: e.y, button: e.button || 'left', buttons: 0, clickCount: e.cc || 1 })
      else if (e.t === 'move') cmd('Input.dispatchMouseEvent', { type: 'mouseMoved', x: e.x, y: e.y, button: e.b & 1 ? 'left' : 'none', buttons: e.b || 0 })
      else if (e.t === 'scroll') cmd('Input.dispatchMouseEvent', { type: 'mouseWheel', x: e.x || 400, y: e.y || 300, deltaX: e.dx || 0, deltaY: e.dy || 0 })
      else if (e.t === 'kd' || e.t === 'ku') dispatchKey(e.t === 'kd' ? 'keyDown' : 'keyUp', e)
      else if (e.t === 'paste') cmd('Input.insertText', { text: e.text || '' })
      else if (e.t === 'getsel') cmd('Runtime.evaluate', { expression: '(getSelection&&getSelection().toString())||""', returnByValue: true }, r => broadcast({ t: 'sel', text: (r && r.result && r.result.value) || '' }))
      else if (e.t === 'nav') { let u = e.url; if (!/^https?:/.test(u)) u = 'https://' + u; cmd('Page.navigate', { url: u }) }
      else if (e.t === 'back') historyGo(-1)
      else if (e.t === 'forward') historyGo(1)
      else if (e.t === 'reload') cmd('Page.reload', {})
      else if (e.t === 'size') applySize(e.w, e.h, e.dpr)
    })
  })
  server.listen(PORT, '0.0.0.0', () => console.log('browser bridge listening on ' + PORT))
})().catch(e => { console.error('bridge fatal:', e.message); process.exit(1) })

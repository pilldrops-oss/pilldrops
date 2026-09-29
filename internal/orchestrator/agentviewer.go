package orchestrator

import (
	"html/template"
	"log"
	"net/http"
)

// The read-only agent viewer served at the root of an agent origin. A single
// self-contained document rather than the SPA: a share-link recipient has no
// portal session, and the CSP allows nothing external.

var agentViewerTmpl = template.Must(template.New("agentviewer").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>Agent · {{.ID}}</title>
<style>
:root{color-scheme:dark;--bg:#0e0e10;--fg:#e8eaed;--dim:#9aa0a6;--line:#26262b;--accent:#8ab4f8;--warn:#f0b357;--err:#f28b82;--ok:#81c995}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
header{position:sticky;top:0;background:var(--bg);border-bottom:1px solid var(--line);padding:14px 18px;display:flex;gap:12px;align-items:baseline;flex-wrap:wrap}
h1{font-size:15px;margin:0;font-weight:600}
.id{color:var(--dim);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}
.pill{margin-left:auto;font-size:12px;color:var(--dim);border:1px solid var(--line);border-radius:999px;padding:2px 10px}
main{max-width:860px;margin:0 auto;padding:18px}
.row{border-left:2px solid var(--line);padding:8px 0 8px 12px;margin:0 0 10px}
.row .k{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:var(--dim)}
.row pre{margin:4px 0 0;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px}
.tool{border-left-color:var(--accent)}
.text{border-left-color:var(--ok)}
.deny{border-left-color:var(--warn)}
.error{border-left-color:var(--err)}
.note{color:var(--dim);border:1px solid var(--line);border-radius:8px;padding:12px 14px;margin:0 0 16px}
.note b{color:var(--fg);font-weight:600}
a{color:var(--accent)}
</style></head>
<body>
<header>
  <h1>Agent activity</h1>
  <span class="id">{{.ID}}</span>
  <span class="pill" id="state">connecting…</span>
</header>
<main>
  <div class="note" id="intro">
    <b>Read-only view.</b> This page shows what the agent is doing. It is a separate
    origin from the dashboard on purpose, so the link you followed cannot act on
    anything else in the account. Queuing work, interrupting a turn and changing the
    spend cap are dashboard actions and are not available here.
  </div>
  <div id="feed"></div>
</main>
<script>
(function(){
  var feed=document.getElementById('feed'), state=document.getElementById('state');
  var lastSeq=0, es=null;

  function setState(t,cls){ state.textContent=t; state.style.color=cls||''; }

  // textContent everywhere, never innerHTML: every string below originates in
  // model output or tool results, which is the least trustworthy content here.
  function row(kind,label,body){
    var d=document.createElement('div'); d.className='row '+kind;
    var k=document.createElement('div'); k.className='k'; k.textContent=label;
    d.appendChild(k);
    if(body){ var p=document.createElement('pre'); p.textContent=body; d.appendChild(p); }
    feed.appendChild(d);
    return d;
  }

  function render(ev,data){
    var d={}; try{ d=JSON.parse(data)||{} }catch(e){ d={} }
    if(ev==='agent.text')       return row('text','answer', d.text||'');
    if(ev==='agent.tool_start') return row('tool','tool · '+(d.tool||'?'), d.detail||'');
    if(ev==='agent.tool_end')   return row('tool','tool done · '+(d.tool||'?'), d.detail||'');
    if(ev==='approval.denied')  return row('deny','blocked · '+(d.tool||'?'), d.detail||'');
    if(ev==='turn.started')     return row('','turn started', d.request||'');
    if(ev==='turn.completed')   return row('','turn finished', d.summary||'');
    if(ev==='turn.failed')      return row('error','turn failed', d.error||'');
    // Unknown event kinds are shown rather than dropped: a viewer that silently
    // hides what it does not recognise is indistinguishable from one that is
    // broken, and this page will lag the server's vocabulary by design.
    return row('','· '+ev, data);
  }

  function connect(){
    // since_seq resumes exactly where this page left off. EventSource also sends
    // Last-Event-ID automatically on its own reconnects; both are honoured server
    // side, so no event is shown twice and none is skipped.
    var url='/v1/agent/events'+(lastSeq?('?since_seq='+lastSeq):'');
    es=new EventSource(url,{withCredentials:true});
    es.onopen=function(){ setState('live','var(--ok)') };
    es.onmessage=function(m){ if(m.lastEventId) lastSeq=parseInt(m.lastEventId,10)||lastSeq; };
    ['agent.text','agent.tool_start','agent.tool_end','approval.denied',
     'turn.started','turn.completed','turn.failed'].forEach(function(name){
      es.addEventListener(name,function(m){
        if(m.lastEventId) lastSeq=parseInt(m.lastEventId,10)||lastSeq;
        render(name,m.data);
        window.scrollTo(0,document.body.scrollHeight);
      });
    });
    es.onerror=function(){
      // EventSource retries on its own; say so rather than implying it is dead.
      setState('reconnecting…','var(--warn)');
    };
  }

  fetch('/v1/agent/status',{credentials:'same-origin'}).then(function(r){
    if(r.status===401){ setState('link expired','var(--err)');
      row('error','this link is no longer valid',
        'Share links expire, and can be revoked by the owner. Ask for a new one.');
      throw new Error('unauthorised'); }
    if(r.status===403){ setState('not permitted','var(--err)');
      row('error','this link cannot read the timeline',
        'The token it carries does not include the agent:read scope.');
      throw new Error('forbidden'); }
    if(!r.ok) throw new Error('status '+r.status);
    return r.json();
  }).then(function(s){
    if(s && s.model) row('','model', String(s.model));
    connect();
  }).catch(function(e){
    if(state.textContent==='connecting…'){ setState('unavailable','var(--err)');
      row('error','cannot reach this agent', String(e && e.message || e)); }
  });
})();
</script>
</body></html>`))

// serveAgentViewer renders the viewer for one sandbox. id comes from the Host
// header, so it must only reach the page through html/template.
func serveAgentViewer(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A mid-stream failure has already sent headers, so logging is all that is left.
	if err := agentViewerTmpl.Execute(w, struct{ ID string }{ID: id}); err != nil {
		log.Printf("agentviewer: template execute failed: %v", err)
	}
}

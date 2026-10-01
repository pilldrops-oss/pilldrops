package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// previewCookieName scopes the granted-access cookie to one port, so the
// browser never offers the :3000 cookie on a :8080 request.
func previewCookieName(id string, port int) string {
	return "pdpv_" + id + "_" + strconv.Itoa(port)
}

// previewToken derives the browser-hop preview token from the shared API key,
// the same value the portal computes. "" when there is no API key, or when the
// sandbox's generation cannot be read — minting a token the gate below would
// refuse hands the caller a link that cannot open.
func (s *Server) previewToken(ctx context.Context, id string, port int) string {
	if s.apiKey == "" {
		return ""
	}
	gen, ok := s.previewGeneration(ctx, id)
	if !ok {
		return ""
	}
	return previewtoken.Browser(previewtoken.BrowserKey(s.apiKey), id, port, gen)
}

// previewGeneration is the revocation generation bound into every browser
// preview token: the sandbox's CURRENT visibility, re-read per request.
//
// # Why visibility revokes preview links, recorded because it was once decided
// # by nobody
//
// A preview token carries no identity. It says "whoever holds this may open
// sandbox X on port P until time T", and nothing else. Before this, flipping a
// sandbox to private closed the API to the rest of the org and left every
// already-pasted preview link serving, for up to BrowserTTL — the one thing a
// person pressing "make private" is asking to stop.
//
// The argument for leaving it alone was that a token with no identity cannot be
// revoked selectively, so revoking locks the creator out with everyone else.
// That argument is answered by making the generation a value the creator can
// move past rather than a wall: the creator holds an API key, mints a fresh
// token under the new generation (GET /v1/sandboxes/{id}/preview-token), and is
// back in one request later. Nobody holding only a link is.
//
// It is deliberately NOT "private means no preview". Private is a normal state,
// not a broken one — a fork child is private by default — so that rule would
// leave forks with no preview at all. The grant is generation-scoped, not
// visibility-gated: a private sandbox previews perfectly well through a token
// minted while it was private.
//
// Sharing (private -> org) revokes too. It is the same event from the other
// side, the share wipe already makes it a state change rather than a widening,
// and one rule beats two.
//
// ok=false means the store could not be read. The caller refuses: this is the
// same fail-closed rule isPublicPreview already applies, and a store outage
// already stops mint, list and create.
func (s *Server) previewGeneration(ctx context.Context, id string) (string, bool) {
	st := s.st.Load()
	if st == nil {
		// Storeless: nothing records a visibility, so mint and verify agree on
		// the default and the gate is exactly the token check it always was.
		return sandbox.VisibilityOrg, true
	}
	row, found, err := s.previewRow(ctx, id)
	if err != nil {
		s.log().ErrorContext(ctx, "preview generation lookup failed, refusing the preview",
			"component", "orchestrator", "sandbox", id, "err", err)
		return "", false
	}
	if !found || row.Visibility == "" {
		// No row, or a row from before visibility existed: the default.
		return sandbox.VisibilityOrg, true
	}
	return row.Visibility, true
}

// previewHopTTL bounds the orchestrator-to-node preview credential so a ?ptok= captured from a log expires.
const previewHopTTL = 5 * time.Minute

// nodePreviewToken mints the credential for the node's own /preview gate.
// Returns "" when nodeToken is unset, and the node then refuses the request.
func (s *Server) nodePreviewToken(ctx context.Context, id string, port int) string {
	if s.nodeToken == "" {
		return ""
	}
	org, _ := s.previewOrgOf(ctx, id)
	// Asymmetric when configured, so a node can verify but not mint.
	if s.orgAssertKey != nil {
		return previewtoken.TokenAsym(s.orgAssertKey, id, port, org, s.now().Add(previewHopTTL))
	}
	return previewtoken.Token([]byte(s.nodeToken), id, port, org)
}

// isPublicPreview reports whether the sandbox preview skips the token gate.
// Read from the shared store so every replica agrees; fails closed on error.
func (s *Server) isPublicPreview(ctx context.Context, id string) bool {
	if st := s.st.Load(); st != nil {
		v, err := s.pcache.get("pub:"+id, previewFresh, s.now(), func() (any, error) { return st.IsPublicPreview(ctx, id) })
		public, _ := v.(bool)
		if err != nil {
			s.log().ErrorContext(ctx, "public-preview lookup failed, treating the sandbox as private",
				"component", "orchestrator", "sandbox", id, "err", err)
			return false
		}
		return public
	}
	v, ok := s.publicSB.Load(id)
	return ok && v.(bool)
}

const previewPrivatePage = `<!doctype html><meta charset="utf-8"><title>Private sandbox</title>
<style>html,body{margin:0;height:100%;display:grid;place-items:center;background:#0e0e10;color:#e8eaed;font-family:system-ui,Segoe UI,Roboto,sans-serif}
.c{max-width:440px;text-align:center;padding:32px}h1{font-size:19px;margin:0 0 10px}p{color:#9aa0a6;font-size:14px;line-height:1.5;margin:0}code{color:#8ab4f8}</style>
<div class="c"><h1>🔒 This sandbox preview is private</h1>
<p>Open it with the <code>viewer_url</code> from the create response (or the dashboard). The link carries an access token; the bare URL isn't reachable. Create with <code>public: true</code> to make a preview shareable without a token.</p>
<p style="margin-top:10px">If this link used to work: changing a sandbox's visibility retracts every preview link issued before the change. Mint a new one with <code>GET /v1/sandboxes/&lt;id&gt;/preview-token?port=&lt;port&gt;</code>.</p></div>`

// previewPlatformPortPage is the refusal for the terminal and desktop ports,
// where public: true does not apply, so the generic page's advice would mislead.
const previewPlatformPortPage = `<!doctype html><meta charset="utf-8"><title>Owner access only</title>
<style>html,body{margin:0;height:100%;display:grid;place-items:center;background:#0e0e10;color:#e8eaed;font-family:system-ui,Segoe UI,Roboto,sans-serif}
.c{max-width:440px;text-align:center;padding:32px}h1{font-size:19px;margin:0 0 10px}p{color:#9aa0a6;font-size:14px;line-height:1.5;margin:0}code{color:#8ab4f8}</style>
<div class="c"><h1>🔒 This port needs the owner's access link</h1>
<p>The terminal and desktop ports give shell access to the sandbox, so they always need an access token from the sandbox's owner, even when the sandbox is public. <code>public: true</code> shares only the app's own ports.</p>
<p style="margin-top:10px">The owner can mint a link with <code>GET /v1/sandboxes/&lt;id&gt;/preview-token?port=&lt;port&gt;</code> or open it from the dashboard.</p></div>`

// authorizePreview is the single preview gate: public sandboxes pass, private
// ones need ?token= (then persisted as a cookie so sub-resources load) or the
// cookie. Writes the 403 page and returns false when refused.
func (s *Server) authorizePreview(w http.ResponseWriter, r *http.Request, id string, port int) bool {
	// Source-network gate first, before any hint that the id is real: an
	// off-network peer gets exactly the answer an unknown sandbox gets, so a
	// restricted org's previews cannot be enumerated from outside its networks.
	if org, ok := s.previewOrgOf(r.Context(), id); ok && !s.peerAllowed(r, org) {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return false
	}
	// Public shares the app, never the platform's own ports: the node hands
	// the terminal hop envd's shell credential, so a tokenless 7681 on a
	// public sandbox was an anonymous shell.
	if !rt.IsPlatformPort(port) && s.isPublicPreview(r.Context(), id) {
		return true
	}
	if s.apiKey == "" {
		// Fail closed: no API key means no gate can be derived.
		if devInsecure() {
			return true
		}
		setPreviewGuards(w.Header(), s.previewGuardDomain(r))
		http.Error(w, "preview gating unavailable: orchestrator has no API key configured", http.StatusServiceUnavailable)
		return false
	}
	// Validate rather than recompute and compare: tokens carry their own expiry.
	key := previewtoken.BrowserKey(s.apiKey)
	// The sandbox's current visibility, not the one it had when the token was
	// minted: this is what retracts a link on unshare. See previewGeneration.
	gen, ok := s.previewGeneration(r.Context(), id)
	if !ok {
		setPreviewGuards(w.Header(), s.previewGuardDomain(r))
		http.Error(w, "preview gating unavailable: cannot read the sandbox to judge who may open it",
			http.StatusServiceUnavailable)
		return false
	}
	ckName := previewCookieName(id, port)
	if previewtoken.BrowserValid(key, r.URL.Query().Get("token"), id, port, gen) {
		// Re-mint for the cookie so the visitor gets a full window, not the link's remainder.
		http.SetCookie(w, &http.Cookie{
			Name: ckName, Value: previewtoken.Browser(key, id, port, gen),
			Path: "/", Secure: true, HttpOnly: true,
			SameSite: http.SameSiteLaxMode, MaxAge: int(previewtoken.BrowserTTL.Seconds()),
		})
		return true
	}
	if ck, err := r.Cookie(ckName); err == nil && previewtoken.BrowserValid(key, ck.Value, id, port, gen) {
		return true
	}
	setPreviewGuards(w.Header(), s.previewGuardDomain(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	page := previewPrivatePage
	if rt.IsPlatformPort(port) {
		page = previewPlatformPortPage
	}
	_, _ = w.Write([]byte(page)) //nolint:errcheck // best-effort: status/headers already committed
	return false
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.previewLimit != nil && s.previewLimit.rps > 0 && !s.previewLimit.allow(httpx.ClientIP(r)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		http.Error(w, "sandbox not found", 404)
		return
	}
	if !s.authorizePreview(w, r, id, portFromPreviewPath(r.URL.Path)) {
		return
	}
	// Read once: the closures below run on the proxy's own request, and the
	// framing policy must stay the one this hostname earned.
	guardDomain := s.previewGuardDomain(r)
	// The preview proxy takes the pinned hop too; falling back to the raw URL
	// on error would defeat the enforcement switch.
	base, hop, err := s.nodeHop(node, hopStream)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		http.Error(w, "bad node url", http.StatusBadGateway)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = hop.Transport
	// The node's own /preview gate requires ?ptok=. Wrap the default director
	// rather than replace it so the scheme/host rewrite stays.
	tok := s.nodePreviewToken(r.Context(), id, portFromPreviewPath(r.URL.Path))
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		// This hop is in the PATH form, /preview/<id>/<port>/..., which is what
		// previewRewrite just restated a host-form preview as. The default
		// director rewrites URL.Host and leaves req.Host alone, so without this
		// the node is handed the caller's 8000-<id>.<domain> Host — and a node
		// with SANDBOXD_SANDBOX_DOMAIN set (deployment.md tells operators to set
		// it) routes by Host first and forwards "the whole request path" to the
		// app, internal prefix and all. Measured on a two-node fleet: the guest
		// was asked for GET /preview/<id>/8000/mark.txt and answered 404.
		req.Host = target.Host
		if tok != "" {
			q := req.URL.Query()
			q.Set("ptok", tok)
			req.URL.RawQuery = q.Encode()
		}
	}
	// Boot-window 502/503 from the node becomes 503 + Retry-After with a
	// negotiated body, so a poller is not told 200 while the app is down.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
			if resp.Body != nil {
				_ = resp.Body.Close() //nolint:errcheck // best-effort: body is about to be replaced below
			}
			body, ctype := readinessBody(resp.Request, id, "starting")
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.StatusCode = http.StatusServiceUnavailable
			resp.Status = "503 Service Unavailable"
			resp.ContentLength = int64(len(body))
			resp.Header = http.Header{}
			resp.Header.Set("Content-Type", ctype)
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
			resp.Header.Set("Retry-After", "2")
			resp.Header.Set("Cache-Control", "no-store")
		}
		// Every response, so an app cannot opt out of the framing protection.
		setPreviewGuards(resp.Header, guardDomain)
		return nil
	}
	// An unreachable node is the same "not ready yet" class.
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, _ error) {
		body, ctype := readinessBody(req, id, "starting")
		setPreviewGuards(w.Header(), guardDomain)
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body) //nolint:errcheck // best-effort: status/headers already committed
	}
	proxy.ServeHTTP(w, r)
}

// readinessBody returns the boot-window body negotiated by Accept: HTML for
// browsers, JSON for everything else.
func readinessBody(req *http.Request, id, state string) (body []byte, contentType string) {
	if req != nil && strings.Contains(req.Header.Get("Accept"), "text/html") {
		return []byte(previewStartingPage), "text/html; charset=utf-8"
	}
	payload := map[string]any{"state": state, "sandbox": id}
	if req != nil {
		if port := portFromPreviewPath(req.URL.Path); port > 0 {
			payload["port"] = port
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{"state":"starting"}`)
	}
	return b, "application/json"
}

// portFromPreviewPath extracts <port> from a /preview/<id>/<port>/ path, or 0.
// proxy's parser is the strict one: it refuses an empty id and a port outside
// 1-65535, both of which this tier's own copy used to hand to authorizePreview.
func portFromPreviewPath(p string) int {
	_, port, _, _ := proxy.ParsePreviewPath(p)
	return port
}

// previewStartingPage is served while the app is still binding its port. It
// polls fast for 60s, then explains and keeps polling slowly.
const previewStartingPage = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Starting your app…</title>
<style>
:root{color-scheme:light dark}
body{margin:0;height:100vh;display:grid;place-items:center;font-family:'Hanken Grotesk',system-ui,-apple-system,sans-serif;background:#fafafa;color:#0a0a0b}
@media(prefers-color-scheme:dark){body{background:#0a0a0b;color:#fafafa}}
.card{text-align:center;max-width:400px;padding:0 24px}
.spin{width:34px;height:34px;margin:0 auto 20px;border:3px solid #d8d8de;border-top-color:#0a0a0b;border-radius:50%;animation:s .8s linear infinite}
@media(prefers-color-scheme:dark){.spin{border-color:#333;border-top-color:#fafafa}}
@keyframes s{to{transform:rotate(360deg)}}
h1{font-size:19px;font-weight:600;margin:0 0 8px}
p{font-size:14px;line-height:1.6;color:#6c6c76;margin:0 0 6px}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;background:rgba(128,128,128,.16);padding:1px 5px;border-radius:4px}
.btn{margin-top:18px;font:inherit;font-size:13px;font-weight:600;cursor:pointer;padding:8px 16px;border-radius:8px;border:1px solid #d8d8de;background:transparent;color:inherit}
@media(prefers-color-scheme:dark){.btn{border-color:#333}}
</style></head><body><div class="card">
<div class="spin" id="spin"></div>
<h1 id="title">Starting your app…</h1>
<p id="msg">Your sandbox is booting and binding its port. This page refreshes automatically — it’ll show your app in a few seconds.</p>
<div id="extra"></div>
</div>
<script>
(function(){
  var KEY='__sbxPreviewStart:'+location.pathname;
  var LIMIT=60000, now=Date.now();
  var started=Number(sessionStorage.getItem(KEY))||0;
  // Fresh start, or a stale timestamp from a much older visit → reset the clock.
  if(!started||now-started>15*60000){started=now;sessionStorage.setItem(KEY,String(started));}
  var elapsed=now-started;
  if(elapsed<LIMIT){
    setTimeout(function(){location.reload();},2000); // fast poll during boot
    return;
  }
  // Gave up waiting: explain instead of spinning forever, but keep a slow poll so
  // a late-binding app still takes over on its own.
  document.getElementById('spin').style.display='none';
  document.getElementById('title').textContent='App isn’t responding yet';
  document.getElementById('msg').innerHTML='Your sandbox is running, but nothing is listening on its preview port (<code>8000</code>) after 60 seconds.';
  window.__sbxRetry=function(){sessionStorage.removeItem(KEY);location.reload();};
  document.getElementById('extra').innerHTML='<p>If this is a web app, make sure it starts a server bound to <code>0.0.0.0:8000</code>. Some templates (like the code interpreter) don’t serve a web app — those have no preview.</p><button class="btn" onclick="window.__sbxRetry()">Retry</button>';
  setTimeout(function(){location.reload();},15000); // slow self-heal poll
})();
</script>
</body></html>`

// previewRow is the sandbox row the preview gate reads, through the preview
// cache (see previewcache.go).
func (s *Server) previewRow(ctx context.Context, id string) (store.ActiveSandboxRow, bool, error) {
	type rowFound struct {
		row   store.ActiveSandboxRow
		found bool
	}
	st := s.st.Load()
	if st == nil {
		return store.ActiveSandboxRow{}, false, nil
	}
	v, err := s.pcache.get("row:"+id, previewFresh, s.now(), func() (any, error) {
		row, found, err := st.GetActiveSandboxRow(ctx, id)
		return rowFound{row, found}, err
	})
	if err != nil {
		return store.ActiveSandboxRow{}, false, err
	}
	rf := v.(rowFound)
	return rf.row, rf.found, nil
}

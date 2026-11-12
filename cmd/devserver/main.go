// Command devserver is the portal: dashboard, public site, and the /v1 API,
// proxied to the orchestrator in fleet mode or run against a local envd
// process for development (make dev-full).

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/apispec"
	"github.com/pilldrops-oss/pilldrops/internal/config"

	"github.com/go-chi/chi/v5"
	chimid "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/mcp"

	// Aliased to keep "store" free as a local name.
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	pdstore "github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
	"github.com/pilldrops-oss/pilldrops/internal/webui"
	pb "github.com/pilldrops-oss/pilldrops/proto/envdpb"
)

// fleetPaths are the API prefixes the portal forwards to the orchestrator.
// A new top-level node resource must be added here or it 404s at the portal.
var fleetPaths = []string{"/v1/sandboxes"}

func isFleetPath(p string) bool {
	for _, pre := range fleetPaths {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// answerWithoutFleet is the local runtime's reply on a control-plane route. It
// can still say what it serves, so the CLI hides what a quick start lacks.
func answerWithoutFleet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/capabilities" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scope":"local","features":[]}`))
		return
	}
	http.Error(w, `{"error":"this requires fleet mode"}`, http.StatusNotImplemented)
}

// registerFleetPassthrough mounts the routes the control plane answers for the
// portal. chi only runs a handler for a matched pattern, so a route the dashboard
// calls that is missing here 404s however healthy the fleet behind it is.
func registerFleetPassthrough(r chi.Router, passthrough http.HandlerFunc) {
	for _, pat := range []string{
		// The node probes what the preview would actually serve.
		"/v1/sandboxes/{id}/preview-status",
		"/v1/sandboxes/{id}/fork", "/v1/sandboxes/{id}/visibility", "/v1/sandboxes/{id}/preview-access",
		"/v1/domains", "/v1/domains/{host}",
		// Volumes live on nodes; the control plane places them and pins creates.
		"/v1/volumes", "/v1/volumes/{id}",
		"/v1/capabilities",
		"/v1/limits",
		"/v1/launch-stats",
		"/v1/whoami",
		"/v1/images",
		"/v1/snapshots",
		"/v1/snapshots/{name}",
		"/v1/sandboxes/{id}/preview-token",
		"/v1/sandboxes/{id}/config",
		"/v1/sandboxes/{id}/checkpoints",
		"/v1/sandboxes/{id}/checkpoints/{name}/restore",
		"/v1/sandboxes/{id}/snapshot",
		"/v1/sandboxes/{id}/standby",
		"/v1/sandboxes/{id}/backups",
		"/v1/sandboxes/{id}/backups/{bkp}/restore",
		"/v1/sandboxes/{id}/forwards",
		"/v1/sandboxes/{id}/forwards/{port}",
	} {
		r.Get(pat, passthrough)
		r.Post(pat, passthrough)
		r.Delete(pat, passthrough)
	}
}

// maskKey renders a key as prefix…suffix without panicking on short keys.
func maskKey(k string) string {
	if len(k) <= 12 {
		return k
	}
	return k[:8] + "..." + k[len(k)-4:]
}

// decodeJSONBody decodes r's body into v. A missing or malformed body leaves
// v at its zero value; callers validate required fields themselves.
func decodeJSONBody(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return
	}
}

// writeBytes writes b as the response body.
func writeBytes(w http.ResponseWriter, b []byte) {
	if _, err := w.Write(b); err != nil {
		log.Printf("writeBytes: %v", err)
	}
}

// writeFprintf is fmt.Fprintf to an HTTP response.
func writeFprintf(w io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		log.Printf("writeFprintf: %v", err)
	}
}

// closeQuietly closes c and logs a failure instead of returning it.
func closeQuietly(c io.Closer, what string) {
	if err := c.Close(); err != nil {
		log.Printf("%s: close: %v", what, err)
	}
}

// keyIDHeader names the portal-authenticated key to the orchestrator, beside
// the org it asserts in X-Org-Override. The orchestrator honours it because
// this hop carries the master key, and signs it on to the node; a client's own
// X-Key-Id is never relayed, exactly as its X-Org-Override is not.
const keyIDHeader = "X-Key-Id"

// portalStreamClient has no overall timeout: http.Client.Timeout also bounds
// reading the body, which would sever an open event stream. Redirects are
// relayed, not followed, so a 303's Set-Cookie (the share-code claim) survives.
var portalStreamClient = newPortalStreamClient()

// portalBufferedClient serves proxyToOrchestrator's request/response exchanges;
// unlike the stream client it carries an overall timeout.
var portalBufferedClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: httpx.NewTransport(),
}

func newPortalStreamClient() *http.Client {
	return &http.Client{
		// A literal &http.Transport{} has no dial timeout and only 2 idle conns per host.
		Transport: httpx.NewTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// proxyToOrchestrator forwards the request to the orchestrator with the
// server's API key and returns the buffered response. owner is the
// authenticated customer; it is a parameter rather than read from the inbound
// headers because this hop carries the orchestrator's master key, so a
// forwarded X-Org-Override would be honoured.
func proxyToOrchestrator(r *http.Request, base, key, owner string) (body []byte, status int, ctype string) {
	var rdr io.Reader
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, 0, ""
		}
		rdr = bytes.NewReader(b)
	}
	target := strings.TrimRight(base, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, rdr)
	if err != nil {
		return nil, 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// Set with "" would send a present-but-blank header, which downstream
	// reads as a named-but-empty org.
	if owner != "" {
		req.Header.Set("X-Org-Override", owner)
		// Who below the org is calling. Derived from the inbound credential,
		// never relayed: an inbound X-Key-Id is a client's claim about itself.
		if id := requestKeyID(r); id != "" {
			req.Header.Set(keyIDHeader, id)
		}
	}
	for _, h := range []string{
		"Content-Type", "Idempotency-Key",
	} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := portalBufferedClient.Do(req)
	if err != nil {
		return nil, 0, ""
	}
	defer closeQuietly(resp.Body, "proxyToOrchestrator")
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, ""
	}
	return b, resp.StatusCode, resp.Header.Get("Content-Type")
}

// streamToOrchestrator forwards a request to the orchestrator and relays the
// response chunk by chunk, flushing after each read, so SSE frames reach the
// client as they arrive. relayHeaders is an allowlist of extra inbound headers
// to carry through; the inbound Authorization is the customer's portal
// credential and must never reach a node.
func streamToOrchestrator(w http.ResponseWriter, r *http.Request, base, key, owner string, relayHeaders ...string) {
	target := strings.TrimRight(base, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// An empty header would read downstream as a named-but-empty org.
	if owner != "" {
		req.Header.Set("X-Org-Override", owner)
		if id := requestKeyID(r); id != "" {
			req.Header.Set(keyIDHeader, id)
		}
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for _, h := range relayHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// portalStreamClient has no overall timeout and does not follow redirects;
	// either would break a live stream.
	resp, err := portalStreamClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"orchestrator unreachable"}`, http.StatusBadGateway)
		return
	}
	defer closeQuietly(resp.Body, "streamToOrchestrator")
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue // must stay chunked, not buffered-with-length
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	// Commit headers before the first body chunk.
	if err := rc.Flush(); err != nil {
		log.Printf("streamToOrchestrator: flushing headers: %v", err)
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// A failed write means the client left; keep draining upstream.
			writeBytes(w, buf[:n])
			if err := rc.Flush(); err != nil {
				log.Printf("streamToOrchestrator: flushing chunk: %v", err)
			}
		}
		if rerr != nil {
			break
		}
	}
}

// firstPort returns the first entry of a create body's ports list, or 0.
func firstPort(raw []byte) int {
	var m struct {
		Ports []int `json:"ports"`
	}
	if json.Unmarshal(raw, &m) == nil && len(m.Ports) > 0 {
		return m.Ports[0]
	}
	return 0
}

// portalSandboxDomain enables subdomain preview URLs (https://3000-<id>.<domain>/).
// Empty means path-form /preview/... on the portal host.
var portalSandboxDomain = strings.ToLower(os.Getenv("PORTAL_SANDBOX_DOMAIN"))

// dashboardOrigin is PORTAL_ORIGIN, the dashboard's own origin when it is not
// on portalSandboxDomain; "" when unset. main refuses anything but one origin.
var dashboardOrigin = strings.TrimRight(os.Getenv("PORTAL_ORIGIN"), "/")

// dashboardOriginError refuses a PORTAL_ORIGIN that is not one bare http(s)
// origin: it becomes link targets and redirect locations.
func dashboardOriginError(origin string) error {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("PORTAL_ORIGIN %q must be one origin such as https://app.example.com", origin)
	}
	return nil
}

// portalLegacyDomains reads PORTAL_LEGACY_SANDBOX_DOMAINS, refusing settings
// that would redirect the dashboard, or the portal's own calls to orchURL, to
// the preview domain.
func portalLegacyDomains(raw, orchURL string) ([]string, error) {
	if err := dashboardOriginError(dashboardOrigin); err != nil {
		return nil, err
	}
	legacy, err := proxy.ParseLegacyDomains(raw, portalSandboxDomain, dashboardOrigin)
	if err != nil {
		return nil, err
	}
	if err := proxy.LegacyHostError(legacy, "PORTAL_ORCHESTRATOR_URL", orchURL); err != nil {
		return nil, err
	}
	proxy.WarnDashboardOnPreviewDomain(legacy, portalSandboxDomain, dashboardOrigin)
	return legacy, nil
}

// previewURL builds the browser-reachable URL for a sandbox's app port: the
// subdomain form when a sandbox domain is configured (always https, TLS
// terminates at the CDN), otherwise the request's own scheme and host.
func previewURL(r *http.Request, id string, port int) string {
	if portalSandboxDomain != "" {
		return fmt.Sprintf("https://%d-%s.%s/", port, id, portalSandboxDomain)
	}
	scheme := "https"
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS == nil {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/preview/%s/%d/", scheme, r.Host, id, port)
}

// previewPathRe matches the path-form preview URL the node or orchestrator emits.
var previewPathRe = regexp.MustCompile(`/preview/([^/]+)/([0-9]+)/?`)

// rebrandPreview rewrites a node preview URL to the portal's public form;
// other URLs pass through.
func rebrandPreview(r *http.Request, u string) string {
	m := previewPathRe.FindStringSubmatch(u)
	if m == nil {
		return u
	}
	port, err := strconv.Atoi(m[2])
	if err != nil || port <= 0 {
		return u
	}
	return previewURL(r, m[1], port)
}

// previewSecret keys the per-sandbox preview token HMAC. Set in main from
// PREVIEW_SECRET, falling back to the orchestrator key.
var previewSecret []byte

// previewToken derives a sandbox's stateless preview access token, so any
// replica can validate it, under gen: the orchestrator re-reads the sandbox's
// generation on every preview request, so minting under any other value here
// would hand the caller a link the gate refuses.
func previewToken(id string, port int, gen string) string {
	return previewtoken.Browser(previewSecret, id, port, gen)
}

// previewGenerationOf is the generation a rebranded sandbox body names: the
// orchestrator's preview_generation, which carries the flip count this tier
// cannot read, else the one visibility and public_preview imply with no flips,
// which is what a body from a node with no orchestrator in front means.
func previewGenerationOf(m map[string]any) string {
	if gen, _ := m["preview_generation"].(string); gen != "" {
		return gen
	}
	vis, _ := m["visibility"].(string)
	public, _ := m["public_preview"].(bool)
	return sandbox.PreviewGeneration(vis, public, 0)
}

// rebrandBody rewrites app_url/url fields in a proxied sandbox response
// (object or array) and injects the preview token so the client can build an
// authenticated URL.
func rebrandBody(r *http.Request, body []byte) []byte {
	rewrite := func(m map[string]any) {
		for _, k := range []string{"app_url", "url"} {
			if s, ok := m[k].(string); ok && s != "" {
				m[k] = rebrandPreview(r, s)
			}
		}
		// Only sandbox-shaped objects (id plus template or status) get a token.
		id, _ := m["id"].(string)
		if id == "" {
			return
		}
		_, hasTmpl := m["template"]
		_, hasStatus := m["status"]
		if !hasTmpl && !hasStatus {
			return
		}
		port := 8000
		if p, ok := m["public_port"].(float64); ok && p > 0 {
			port = int(p)
		}
		// The token is bound to the declared port; a client probing another
		// port mints its own via GET /v1/sandboxes/{id}/preview-token?port=N.
		tok := previewToken(id, port, previewGenerationOf(m))
		if tok == "" {
			return
		}
		m["cdp_token"] = tok
		m["cdp_token_port"] = port
		base := previewURL(r, id, port)
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		m["viewer_url"] = base + sep + "token=" + tok
		if t, _ := m["template"].(string); t == "browser" {
			ws := strings.Replace(strings.Replace(strings.TrimRight(base, "/"), "https://", "wss://", 1), "http://", "ws://", 1)
			m["cdp_url"] = ws + "/cdp?token=" + tok
		}
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil && obj != nil {
		rewrite(obj)
		if nb, err := json.Marshal(obj); err == nil {
			return nb
		}
		return body
	}
	var arr []map[string]any
	if json.Unmarshal(body, &arr) == nil && arr != nil {
		for _, m := range arr {
			rewrite(m)
		}
		if nb, err := json.Marshal(arr); err == nil {
			return nb
		}
	}
	return body
}

// slugLookup resolves a slug host against the shared store; nil without one.
type slugLookup func(ctx context.Context, slug string) (id string, port int, ok bool)

// storeSlugLookup answers from the sandboxes table; the port defaults to 8000.
func storeSlugLookup(st *pdstore.Store) slugLookup {
	if st == nil {
		return nil
	}
	return func(ctx context.Context, slug string) (string, int, bool) {
		rec, ok, err := st.SandboxBySlug(ctx, slug)
		if err != nil || !ok {
			return "", 0, false
		}
		port := rec.PublicPort
		if port == 0 {
			port = 8000
		}
		return rec.ID, port, true
	}
}

// hostPreview serves sandbox apps on <port>-<id>.<domain> hosts, and on
// <slug>.<domain> when bySlug knows the slug, by rewriting the request into
// the /preview path for the orchestrator reverse proxy. Any other host,
// including the apex, www and an unknown slug, falls through to next.
func hostPreview(domain string, rp http.Handler, next http.Handler, bySlug slugLookup) http.Handler {
	if domain == "" || rp == nil {
		return next
	}
	suffix := "." + domain
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		bare := strings.TrimSuffix(host, suffix)
		if bare == host || bare == "" || strings.Contains(bare, ".") {
			next.ServeHTTP(w, req)
			return
		}
		var id string
		var port int
		if slug, isSlug := proxy.ParseSlugHost(host, domain); isSlug {
			var ok bool
			if bySlug != nil {
				id, port, ok = bySlug(req.Context(), slug)
			}
			if !ok {
				next.ServeHTTP(w, req)
				return
			}
		} else {
			dash := strings.IndexByte(bare, '-')
			if dash <= 0 {
				next.ServeHTTP(w, req)
				return
			}
			p, err := strconv.Atoi(bare[:dash])
			id = bare[dash+1:]
			if err != nil || p <= 0 || p > 65535 || id == "" {
				next.ServeHTTP(w, req)
				return
			}
			port = p
		}
		req2 := req.Clone(req.Context())
		req2.URL.Path = fmt.Sprintf("/preview/%s/%d%s", id, port, req.URL.Path)
		if req.URL.Path == "" || req.URL.Path == "/" {
			req2.URL.Path = fmt.Sprintf("/preview/%s/%d/", id, port)
		}
		req2.URL.RawPath = ""
		rp.ServeHTTP(w, req2)
	})
}

// portalHandler is the portal's host routing: legacy preview hosts redirect,
// preview hosts go to the orchestrator, and everything else to the site,
// except that with the dashboard elsewhere no other host under the sandbox
// domain reaches it.
func portalHandler(legacy *proxy.LegacyDomains, rp, site http.Handler, bySlug slugLookup) http.Handler {
	return legacy.Wrap(hostPreview(portalSandboxDomain, rp,
		legacy.SandboxDomainOnly(portalSandboxDomain, dashboardOrigin, site), bySlug))
}

// pathPreviewRedirect sends a browser navigating the path form to the
// sandbox's own host, so sandbox code never runs on the portal's origin.
// Host-form requests reach rp through hostPreview, not through here.
func pathPreviewRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if target, ok := proxy.PathPreviewTarget(r, portalSandboxDomain, nil); ok {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isInfraPreviewErr reports whether a preview error body came from the
// platform rather than the customer's app; only those get the branded page.
func isInfraPreviewErr(body string) bool {
	for _, p := range []string{
		"sandbox not found",
		"no app listening",
		"sandbox unavailable:",
		"App is not responding on port",
		"preview upstream unreachable",
		"bad node url",
		"bad preview path",
		"not a valid sandbox host",
	} {
		if strings.HasPrefix(body, p) {
			return true
		}
	}
	return false
}

// previewErrorLinks builds the footer links to the dashboard: PORTAL_ORIGIN
// when set, else the sandbox domain's apex (the page is served on a sandbox
// host, so hrefs must be absolute), or none when neither is known: a wrong
// destination is worse than no button.
func previewErrorLinks() string {
	base := dashboardOrigin
	if base == "" && portalSandboxDomain != "" {
		base = "https://" + portalSandboxDomain
	}
	if base == "" {
		return ""
	}
	base = html.EscapeString(base)
	return `<div class="btns">` +
		`<a class="b solid" href="` + base + `/dashboard">Open dashboard</a>` +
		`<a class="b" href="` + base + `/docs">API reference</a>` +
		`</div>`
}

// previewErrorPage renders the branded preview error page. kind is "gone"
// (the sandbox no longer exists) or "starting" (the page auto-retries).
func previewErrorPage(status int, kind, detail string) string {
	headline, sub, retry := "", "", ""
	if kind == "gone" {
		headline = "This sandbox is gone."
		sub = "Sandboxes are ephemeral by design — this one finished its work, hit its timeout, or was destroyed. Its run history and metered usage are preserved in the dashboard."
	} else {
		headline = "The app here isn’t answering — yet."
		sub = "The sandbox may still be booting or installing dependencies, or nothing is listening on this port. This page retries automatically."
		retry = `<div class="retry"><span class="spin"></span>retrying in <b id="n">4</b>s</div>
<script>
var tries=+(sessionStorage.getItem('pd.retry')||0);
if(tries<40){sessionStorage.setItem('pd.retry',tries+1);var n=4,el=document.getElementById('n');
setInterval(function(){n--;el.textContent=n>0?n:0;if(n<=0)location.reload()},1000)}
else{document.querySelector('.retry').innerHTML='stopped auto-retrying — refresh manually'}
</script>`
	}
	r := strings.NewReplacer(
		"{{STATUS}}", strconv.Itoa(status),
		"{{HEADLINE}}", headline,
		"{{SUB}}", sub,
		"{{RETRY}}", retry,
		"{{LINKS}}", previewErrorLinks(),
		"{{DETAIL}}", html.EscapeString(strings.TrimSpace(detail)),
	)
	return r.Replace(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>{{STATUS}} · PillDrops sandbox</title><style>
:root{--bg:#f7f5f0;--surf:#fff;--ink:#141310;--sub:#565448;--dim:#8b887c;--hair:rgba(20,18,12,.16);--acc:#0c8a56}
@media(prefers-color-scheme:dark){:root{--bg:#131210;--surf:#1a1815;--ink:#f4f1e8;--sub:#a8a294;--dim:#7a7567;--hair:rgba(244,240,230,.14);--acc:#35c98a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font-family:'Hanken Grotesk',system-ui,sans-serif;
min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;
background-image:radial-gradient(var(--hair) 1px,transparent 1px);background-size:26px 26px}
.card{max-width:560px;width:100%;background:var(--surf);border:1px solid var(--hair);border-radius:2px;padding:44px 44px 36px}
.brand{display:flex;align-items:center;gap:9px;font-family:ui-monospace,monospace;font-weight:700;font-size:15px;margin-bottom:30px}
.brand .sq{width:18px;height:18px;border-radius:5px;border:3px solid var(--acc)}
.brand em{font-style:normal;text-decoration:underline;text-underline-offset:3px;text-decoration-color:var(--acc)}
.code{font-family:ui-monospace,monospace;font-size:64px;font-weight:700;letter-spacing:-.04em;line-height:1;margin:0 0 14px;color:var(--ink)}
.code span{color:var(--acc)}
h1{font-size:20px;margin:0 0 10px;font-family:ui-monospace,monospace;letter-spacing:-.02em}
p{margin:0 0 22px;color:var(--sub);font-size:14.5px;line-height:1.7}
.host{font-family:ui-monospace,monospace;font-size:12px;color:var(--dim);border:1px dashed var(--hair);border-radius:2px;padding:9px 12px;margin:0 0 22px;word-break:break-all}
.host b{color:var(--acc);font-weight:600}
.btns{display:flex;gap:10px;flex-wrap:wrap;margin-bottom:6px}
a.b{display:inline-block;padding:10px 18px;border-radius:2px;font-size:13.5px;font-weight:600;text-decoration:none;border:1px solid var(--hair);color:var(--ink)}
a.b.solid{background:var(--ink);color:var(--bg);border-color:var(--ink)}
.retry{margin-top:20px;font-family:ui-monospace,monospace;font-size:12.5px;color:var(--dim);display:flex;gap:9px;align-items:center}
.spin{width:11px;height:11px;border:2px solid var(--hair);border-top-color:var(--acc);border-radius:50%;animation:s 1s linear infinite}
@keyframes s{to{transform:rotate(360deg)}}
details{margin-top:18px;font-size:12px;color:var(--dim)}summary{cursor:pointer;font-family:ui-monospace,monospace}
pre{white-space:pre-wrap;word-break:break-word;background:var(--bg);border:1px solid var(--hair);border-radius:2px;padding:10px;font-size:11.5px}
</style></head><body><div class="card">
<div class="brand"><span class="sq"></span>Pill<em>Drops</em></div>
<div class="code">{{STATUS}}<span>.</span></div>
<h1>{{HEADLINE}}</h1>
<p>{{SUB}}</p>
<div class="host" id="h"></div>
{{LINKS}}
{{RETRY}}
<details><summary>technical detail</summary><pre>{{DETAIL}}</pre></details>
</div><script>
(function(){var m=location.hostname.match(/^(\d+)-(.+?)\./);
document.getElementById('h').innerHTML=m?'port <b>'+m[1]+'</b> on sandbox <b>'+m[2]+'</b>':location.hostname})()
</script></body></html>`)
}

// previewErrorIntercept swaps platform preview errors for the branded page on
// browser requests. App responses and non-browser clients pass through.
func previewErrorIntercept(resp *http.Response) error {
	if resp.StatusCode < 400 || resp.Request == nil {
		return nil
	}
	if !strings.Contains(resp.Request.Header.Get("Accept"), "text/html") {
		return nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return nil
	}
	closeQuietly(resp.Body, "previewErrorIntercept")
	if !isInfraPreviewErr(string(b)) {
		resp.Body = io.NopCloser(bytes.NewReader(b)) // the app's own error, untouched
		return nil
	}
	kind := "starting"
	if resp.StatusCode == http.StatusNotFound {
		kind = "gone"
	}
	page := previewErrorPage(resp.StatusCode, kind, string(b))
	resp.Body = io.NopCloser(strings.NewReader(page))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", strconv.Itoa(len(page)))
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Cache-Control", "no-store")
	return nil
}

// previewReverseProxy forwards /preview/{id}/{port}/... to the orchestrator,
// which reverse-proxies to the owning node. No bearer is attached: the
// orchestrator's /preview handler is public. A full ReverseProxy keeps the
// stream unbuffered so WebSockets and HMR pass through. Returns nil if
// orchURL is unparseable.
func previewReverseProxy(orchURL string) http.Handler {
	base, err := url.Parse(strings.TrimRight(orchURL, "/"))
	if err != nil || base.Host == "" {
		return nil
	}
	rp := httputil.NewSingleHostReverseProxy(base)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		// Overwritten, not passed through: it is how the orchestrator tells a
		// preview host rewritten here from the path form a browser opened.
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set(proxy.PreviewHostHeader, req.Host)
		req.Host = base.Host
	}
	rp.ModifyResponse = previewErrorIntercept
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		// Nothing upstream set Referrer-Policy on this path, and the URL still
		// carries ?token=.
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.Contains(req.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			if _, err := io.WriteString(w, previewErrorPage(http.StatusBadGateway, "starting", "preview upstream unreachable: "+err.Error())); err != nil {
				log.Printf("previewReverseProxy: writing error page: %v", err)
			}
			return
		}
		http.Error(w, "preview upstream unreachable: "+err.Error(), http.StatusBadGateway)
	}
	return rp
}

// portalCookie is the login-set session cookie the browser dashboard sends on
// /v1 and SSE calls. Matches internal/webui.SessionCookie.
const portalCookie = "sandboxd_session"

// Broadcaster fans bytes out to SSE subscribers, dropping for a subscriber
// that is not keeping up. Duplicated here because internal/api pulls
// Linux-only deps.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan []byte]struct{})}
}
func (lb *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	lb.mu.Lock()
	lb.subs[ch] = struct{}{}
	lb.mu.Unlock()
	return ch
}
func (lb *Broadcaster) Unsubscribe(ch chan []byte) {
	lb.mu.Lock()
	delete(lb.subs, ch)
	lb.mu.Unlock()
}
func (lb *Broadcaster) Send(data []byte) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for ch := range lb.subs {
		select {
		case ch <- data:
		default:
		}
	}
}

type dashLogCore struct {
	zapcore.Core
	lb *Broadcaster
}

func (c *dashLogCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	err := c.Core.Write(entry, fields)
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	data, merr := json.Marshal(map[string]interface{}{
		"level": entry.Level.String(), "time": entry.Time.Format(time.RFC3339Nano),
		"msg": entry.Message, "fields": enc.Fields,
	})
	if merr == nil {
		c.lb.Send(data)
	}
	return err
}
func (c *dashLogCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}
	return ce
}
func (c *dashLogCore) With(fields []zapcore.Field) zapcore.Core {
	return &dashLogCore{Core: c.Core.With(fields), lb: c.lb}
}

func newDashLogger(base *zap.Logger) (*zap.Logger, *Broadcaster) {
	lb := NewBroadcaster()
	return zap.New(&dashLogCore{Core: base.Core(), lb: lb}), lb
}

// APIKey is a dashboard-minted API key, stored hash-at-rest. Owner binds it
// to its creator ("u:<id>") so the key resolves to the same tenancy as every
// other owner-scoped read.
type APIKey struct {
	ID        string `json:"id"`
	Key       string `json:"key,omitempty"` // set only in the Create response, never persisted
	KeyHash   string `json:"key_hash,omitempty"`
	KeyMasked string `json:"masked_key"`
	Name      string `json:"name"`
	Owner     string `json:"owner,omitempty"` // "u:<id>"; "" for the seeded dev key (legacy k:* tenancy)
	LastUsed  string `json:"last_used"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

type APIKeyStore struct {
	mu     sync.RWMutex
	path   string
	keys   map[string]*APIKey // ID -> record
	byHash map[string]string  // sha256(key) hex -> ID
}

// LoadAPIKeys loads the persisted key store at path. A missing or unreadable
// file yields an empty store so the portal can still start.
func LoadAPIKeys(path string) *APIKeyStore {
	ks := &APIKeyStore{path: path, keys: map[string]*APIKey{}, byHash: map[string]string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("apikeys: could not read key store %s: %v (starting with empty store)", path, err)
		}
		return ks
	}
	var loaded map[string]*APIKey
	if err := json.Unmarshal(raw, &loaded); err != nil {
		log.Printf("apikeys: corrupt key store %s: %v (starting with empty store)", path, err)
		return ks
	}
	// A literal null file unmarshals to a nil map; Create would panic on it.
	if loaded == nil {
		loaded = map[string]*APIKey{}
	}
	ks.keys = loaded
	// A null entry unmarshals to a nil *APIKey that List/Delete/Touch would dereference.
	for id, k := range ks.keys {
		if k == nil {
			delete(ks.keys, id)
			continue
		}
		if k.KeyHash != "" {
			ks.byHash[k.KeyHash] = id
		}
	}
	return ks
}

// List returns the keys owned by owner, without the full key or hash. The
// seeded dev key (Owner "") is never listed.
func (ks *APIKeyStore) List(owner string) []*APIKey {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	out := make([]*APIKey, 0)
	if owner == "" {
		return out
	}
	for _, k := range ks.keys {
		if k.Owner != owner {
			continue
		}
		masked := *k
		masked.Key = ""
		masked.KeyHash = ""
		out = append(out, &masked)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Create mints a dashboard API key bound to owner and persists its hash. The
// returned Key is shown exactly once and cannot be retrieved again.
func (ks *APIKeyStore) Create(name, owner string) (*APIKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	fullKey := "sf_" + randomHex(24)
	id := randomHex(8)
	hash := hashToken(fullKey)
	stored := &APIKey{
		ID: id, KeyHash: hash, KeyMasked: maskKey(fullKey),
		// LastUsed stays "" until first use; the SPA renders that as Never.
		Name: name, Owner: owner, LastUsed: "", CreatedBy: "admin",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	ks.keys[id] = stored
	ks.byHash[hash] = id
	if err := ks.save(); err != nil {
		// Roll back so memory matches the file the caller was told was not written.
		delete(ks.keys, id)
		delete(ks.byHash, hash)
		return nil, err
	}
	resp := *stored
	resp.Key = fullKey // shown once
	resp.KeyHash = ""  // internal only
	return &resp, nil
}

// Delete removes key id if owner owns it. Returns (false, nil) when missing or
// foreign-owned, (false, err) when persistence failed; memory is rolled back
// then so the key does not resurrect on restart.
func (ks *APIKeyStore) Delete(id, owner string) (bool, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	k, ok := ks.keys[id]
	if !ok || owner == "" || k.Owner != owner {
		return false, nil
	}
	delete(ks.keys, id)
	delete(ks.byHash, k.KeyHash)
	if err := ks.save(); err != nil {
		// The file still has this key; keep memory consistent with disk.
		ks.keys[id] = k
		ks.byHash[k.KeyHash] = id
		return false, err
	}
	return true, nil
}

// lookupByHash resolves a presented token to its stored record, if any.
func (ks *APIKeyStore) lookupByHash(token string) (*APIKey, bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	id, ok := ks.byHash[hashToken(token)]
	if !ok {
		return nil, false
	}
	return ks.keys[id], true
}

// Validate reports whether token is a known key.
func (ks *APIKeyStore) Validate(token string) bool {
	_, ok := ks.lookupByHash(token)
	return ok
}

// Touch records LastUsed in memory only, so request traffic never churns the file.
func (ks *APIKeyStore) Touch(token string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	id, ok := ks.byHash[hashToken(token)]
	if !ok {
		return
	}
	ks.keys[id].LastUsed = time.Now().Format(time.RFC3339)
}

// OwnerOf returns the owner bound to token, or "" for an unknown token or the
// seeded dev key; callers then fall back to apiKeyOwner.
func (ks *APIKeyStore) OwnerOf(token string) string {
	k, ok := ks.lookupByHash(token)
	if !ok {
		return ""
	}
	return k.Owner
}

// seedDevKey installs the bootstrap credential derived from PORTAL_API_KEY.
// It bypasses Create/save so it is never persisted or listed, and Owner stays
// "" so ownerID falls through to the legacy apiKeyOwner hash. A previous
// "dev" hash is removed first, or a rotated-out key would keep authenticating.
func (ks *APIKeyStore) seedDevKey(devKey string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if old, ok := ks.keys["dev"]; ok && old.KeyHash != "" {
		delete(ks.byHash, old.KeyHash)
	}
	hash := hashToken(devKey)
	ks.keys["dev"] = &APIKey{
		ID: "dev", KeyHash: hash, KeyMasked: maskKey(devKey),
		Name: "portal-default", Owner: "", LastUsed: "", CreatedBy: "system",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	ks.byHash[hash] = "dev"
}

// save writes the store atomically via temp file and rename, with 0600 perms.
// The "dev" entry is never written to disk. Callers hold ks.mu.
func (ks *APIKeyStore) save() error {
	persisted := make(map[string]*APIKey, len(ks.keys))
	for id, k := range ks.keys {
		if id == "dev" {
			continue
		}
		persisted[id] = k
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	tmp := ks.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ks.path)
}

// newAPIAuth builds the auth middleware for /v1 and /dashboard/api. It accepts
// a bearer or X-API-Key token or the portal session cookie, and allows
// anonymous access only when requireAuth is false (local dev).
func newAPIAuth(keyStore *APIKeyStore, requireAuth bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := presentedToken(r)
			if token == "" {
				if requireAuth {
					http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r) // local dev: anonymous allowed
				return
			}
			// Hash then map lookup is not a timing oracle for the key; there is
			// no raw key to compare against.
			valid := keyStore.Validate(token)
			if valid {
				keyStore.Touch(token)
			}
			if !valid {
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hostStub is the local GET /v1/host. It counts every tenant's sandboxes, so
// like the node's it answers only the operator: here, the PORTAL_API_KEY holder.
func hostStub(mgr *DevManager, portalKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(presentedToken(r)), []byte(portalKey)) != 1 {
			http.Error(w, `{"error":"operator role required"}`, http.StatusForbidden)
			return
		}
		// Full HostResp shape; a partial object crashes the page.
		mgr.mu.RLock()
		active := len(mgr.sandboxes)
		mgr.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		writeFprintf(w, `{"cpu_cores":%d,"cpu_percent":0,"load_avg":[0,0,0],`+
			`"mem_total_mb":0,"mem_used_mb":0,"mem_available_mb":0,`+
			`"disk_total_gb":0,"disk_used_gb":0,"disk_free_gb":0,`+
			`"sandboxes_active":%d,"vcpu_committed":0,"mem_committed_mb":0}`,
			runtime.NumCPU(), active)
	}
}

// registerAPIKeyRoutes mounts GET/POST /v1/keys and DELETE /v1/keys/{id},
// scoped to ownerID(r).
func registerAPIKeyRoutes(r chi.Router, keyStore *APIKeyStore, ownerID func(*http.Request) string) {
	r.Get("/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, keyStore.List(ownerID(r)))
	})
	r.Post("/v1/keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		decodeJSONBody(r, &body)
		if body.Name == "" {
			body.Name = "default"
		}
		k, err := keyStore.Create(body.Name, ownerID(r))
		if err != nil {
			http.Error(w, `{"error":"could not persist key"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		writeJSON(w, k) // full key shown only on creation
	})
	r.Delete("/v1/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		ok, err := keyStore.Delete(chi.URLParam(r, "id"), ownerID(r))
		if err != nil {
			// Found and owned, but the deletion did not persist.
			http.Error(w, `{"error":"could not persist key deletion"}`, http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, `{"error":"key not found"}`, 404)
			return
		}
		w.WriteHeader(204)
	})
}

type UsagePoint struct {
	Time       string  `json:"time"`
	Concurrent int     `json:"concurrent"`
	StartRate  float64 `json:"start_rate"`
}

// UsageTracker tracks sandbox lifecycle metrics over time.
type UsageTracker struct {
	mu           sync.RWMutex
	points       []UsagePoint // time-series data, last 24h
	totalStarts  int64
	totalSeconds float64
	vcpuHours    float64
	ramGBHours   float64
	startTimes   []time.Time // recent starts for rate calc
}

func NewUsageTracker() *UsageTracker {
	return &UsageTracker{}
}

func (u *UsageTracker) RecordStart() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.totalStarts++
	u.startTimes = append(u.startTimes, time.Now())
	cutoff := time.Now().Add(-5 * time.Minute)
	for len(u.startTimes) > 0 && u.startTimes[0].Before(cutoff) {
		u.startTimes = u.startTimes[1:]
	}
}

func (u *UsageTracker) RecordDestroy(durationSec float64, cpuCores int, ramMB int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.totalSeconds += durationSec
	u.vcpuHours += durationSec * float64(cpuCores) / 3600
	u.ramGBHours += durationSec * float64(ramMB) / 1024 / 3600
}

// startRate is starts per second over the last five seconds. The caller holds u.mu.
func (u *UsageTracker) startRate() float64 {
	cutoff := time.Now().Add(-5 * time.Second)
	recent := 0
	for _, t := range u.startTimes {
		if t.After(cutoff) {
			recent++
		}
	}
	return float64(recent) / 5.0
}

func (u *UsageTracker) Snapshot(concurrent int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	pt := UsagePoint{
		Time: time.Now().Format(time.RFC3339), Concurrent: concurrent, StartRate: u.startRate(),
	}
	u.points = append(u.points, pt)
	if len(u.points) > 1440 {
		u.points = u.points[len(u.points)-1440:]
	}
}

func (u *UsageTracker) Stats() map[string]interface{} {
	u.mu.RLock()
	defer u.mu.RUnlock()
	peak := 0
	for _, p := range u.points {
		if p.Concurrent > peak {
			peak = p.Concurrent
		}
	}
	return map[string]interface{}{
		"total_starts":    u.totalStarts,
		"total_seconds":   u.totalSeconds,
		"vcpu_hours":      u.vcpuHours,
		"ram_gb_hours":    u.ramGBHours,
		"peak_concurrent": peak,
		"start_rate":      u.startRate(),
		"points":          u.points,
	}
}

type ExecEntry struct {
	SandboxID string `json:"sandbox_id"`
	Cmd       string `json:"cmd"`
	ExitCode  int    `json:"exit_code"`
	Duration  int    `json:"duration_ms"`
	Timestamp string `json:"timestamp"`
}

// ExecHistory keeps the last 500 command executions across sandboxes.
type ExecHistory struct {
	mu      sync.RWMutex
	entries []ExecEntry
}

func NewExecHistory() *ExecHistory { return &ExecHistory{} }

func (h *ExecHistory) Record(sbID, cmd string, exitCode, durationMs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, ExecEntry{
		SandboxID: sbID, Cmd: cmd, ExitCode: exitCode,
		Duration: durationMs, Timestamp: time.Now().Format(time.RFC3339),
	})
	if len(h.entries) > 500 {
		h.entries = h.entries[len(h.entries)-500:]
	}
}

func (h *ExecHistory) BySandbox(sbID string) []ExecEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []ExecEntry
	for _, e := range h.entries {
		if e.SandboxID == sbID {
			out = append(out, e)
		}
	}
	return out
}

// DevSandbox is a sandbox backed by the shared local envd process.
type DevSandbox struct {
	ID       string
	Template string
	UserID   string
	Port     int
	BootType string
	BootMs   int64
	WorkDir  string // isolated workspace directory
	// BootEnv is the create-time env ("NAME=VALUE"), where a customer's
	// provider key arrives.
	BootEnv   []string
	CreatedAt time.Time
	ExpiresAt time.Time
	conn      *grpc.ClientConn
	client    pb.EnvdServiceClient
	cancel    context.CancelFunc

	// Live status + captured process output (guarded by logMu).
	logMu    sync.Mutex
	status   string // running, exited, crashed
	exitCode int
	logLines []string
	// appStarted: a start command ran, so the preview serves the user app
	// rather than the built-in surface.
	appStarted bool
	// appPgid is the start command's process group, killed on Destroy.
	appPgid int
}

// markAppStarted records that a user app owns the sandbox's port from now on.
func (sb *DevSandbox) markAppStarted() {
	sb.logMu.Lock()
	sb.appStarted = true
	sb.logMu.Unlock()
}

func (sb *DevSandbox) appStartedSnapshot() bool {
	sb.logMu.Lock()
	defer sb.logMu.Unlock()
	return sb.appStarted
}

const maxSandboxLogLines = 2000

func (sb *DevSandbox) appendLog(line string) {
	sb.logMu.Lock()
	defer sb.logMu.Unlock()
	sb.logLines = append(sb.logLines, line)
	if len(sb.logLines) > maxSandboxLogLines {
		sb.logLines = sb.logLines[len(sb.logLines)-maxSandboxLogLines:]
	}
}

func (sb *DevSandbox) logsSnapshot() []string {
	sb.logMu.Lock()
	defer sb.logMu.Unlock()
	out := make([]string, len(sb.logLines))
	copy(out, sb.logLines)
	return out
}

func (sb *DevSandbox) setStatus(s string, code int) {
	sb.logMu.Lock()
	defer sb.logMu.Unlock()
	sb.status = s
	sb.exitCode = code
}

// statusSnapshot returns the status (defaulting to "running") and exit code.
func (sb *DevSandbox) statusSnapshot() (string, int) {
	sb.logMu.Lock()
	defer sb.logMu.Unlock()
	if sb.status == "" {
		return "running", sb.exitCode
	}
	return sb.status, sb.exitCode
}

// DevManager runs sandboxes against the shared local envd process.
type DevManager struct {
	mu            sync.RWMutex
	sandboxes     map[string]*DevSandbox
	envdBin       string
	nextPort      atomic.Int32
	maxActive     int
	poolSize      int
	maxConcurrent int
	templates     []string
	log           *zap.Logger
	createCount   atomic.Int64
	// envdToken authenticates this manager to the envd it started, which
	// refuses to serve a routable port without a token hash.
	envdToken string
}

// envdTokenCreds sends the envd access token on every call.
type envdTokenCreds string

func (t envdTokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{sbtoken.MetadataKey: string(t)}, nil
}

func (envdTokenCreds) RequireTransportSecurity() bool { return false }

func NewDevManager(envdBin string, log *zap.Logger) *DevManager {
	m := &DevManager{
		sandboxes:     make(map[string]*DevSandbox),
		envdBin:       envdBin,
		maxActive:     50,
		poolSize:      2,
		maxConcurrent: 10,
		templates:     []string{"base", "docker", "code-interpreter", "python-3.11", "browser", "desktop", "playwright", "node", "go", "rust"},
		log:           log,
	}
	m.nextPort.Store(7000)
	return m
}

// freePort returns the next port in the dev range that nothing on this host
// is listening on; a left-over or unrelated server would otherwise answer
// for the new sandbox's app.
func (m *DevManager) freePort() int {
	for {
		p := int(m.nextPort.Add(1))
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil { //nolint:noctx // a bind probe, closed at once
			_ = ln.Close()
			return p
		}
	}
}

func (m *DevManager) Create(ctx context.Context, template, userID string, timeout time.Duration, bootEnv []string) (*DevSandbox, error) {
	m.mu.RLock()
	count := len(m.sandboxes)
	m.mu.RUnlock()

	if count >= m.maxActive {
		return nil, fmt.Errorf("max sandboxes reached (%d)", m.maxActive)
	}

	start := time.Now()
	assignedPort := m.freePort()
	sandboxID := genID()

	workDir := filepath.Join(os.TempDir(), "sandbox-workspaces", sandboxID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace dir: %w", err)
	}

	// grpc.NewClient does not dial synchronously, so wait for READY explicitly.
	addr := "localhost:6001"
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(envdTokenCreds(m.envdToken)))
	if err != nil {
		return nil, fmt.Errorf("connect to envd: %w", err)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
	defer dialCancel()
	conn.Connect()
	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			break
		}
		if !conn.WaitForStateChange(dialCtx, state) {
			closeQuietly(conn, "DevManager.Create dial timeout")
			return nil, fmt.Errorf("connect to envd: %w", dialCtx.Err())
		}
	}

	client := pb.NewEnvdServiceClient(conn)

	resp, err := client.Health(ctx, &pb.HealthRequest{})
	if err != nil || !resp.Ready {
		closeQuietly(conn, "DevManager.Create")
		return nil, fmt.Errorf("envd not ready: %w", err)
	}

	bootMs := time.Since(start).Milliseconds()

	timerCtx, cancel := context.WithCancel(context.Background())
	sb := &DevSandbox{
		ID:        sandboxID,
		Template:  template,
		UserID:    userID,
		Port:      assignedPort,
		BootType:  "dev",
		BootMs:    bootMs,
		BootEnv:   append([]string(nil), bootEnv...),
		WorkDir:   workDir,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(timeout),
		conn:      conn,
		client:    client,
		cancel:    cancel,
	}

	go func() {
		select {
		case <-time.After(timeout):
			m.log.Info("sandbox_timeout", zap.String("id", sandboxID))
			m.Destroy(context.Background(), sandboxID)
		case <-timerCtx.Done():
		}
	}()

	m.mu.Lock()
	m.sandboxes[sandboxID] = sb
	m.mu.Unlock()
	m.createCount.Add(1)

	m.log.Info("sandbox_created",
		zap.String("id", sandboxID),
		zap.String("template", template),
		zap.Int64("boot_ms", bootMs),
		zap.String("user", userID),
	)

	return sb, nil
}

// withBootEnv prefixes cmd with the sandbox's create-time environment. envd's
// Execute RPC has no env field, so the variables ride in the command text.
// The inner sh -c is required: without it the outer shell expands $VAR before
// env runs.
func withBootEnv(bootEnv []string, cmd string) string {
	if len(bootEnv) == 0 {
		return cmd
	}
	var b strings.Builder
	b.WriteString("env ")
	for _, kv := range bootEnv {
		b.WriteString(shellQuoteDev(kv))
		b.WriteByte(' ')
	}
	b.WriteString("sh -c ")
	b.WriteString(shellQuoteDev(cmd))
	return b.String()
}

// shellQuoteDev single-quotes s so an opaque provider key cannot become shell syntax.
func shellQuoteDev(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (m *DevManager) Exec(ctx context.Context, sandboxID, cmd, cwd string, timeout time.Duration) (string, string, int, bool, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return "", "", -1, false, fmt.Errorf("sandbox not found")
	}

	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(sb.WorkDir, cwd)
	}

	stream, err := sb.client.Execute(ctx, &pb.ExecuteRequest{
		Cmd:            withBootEnv(sb.BootEnv, cmd),
		Cwd:            cwd,
		TimeoutSeconds: int32(timeout.Seconds()),
	})
	if err != nil {
		return "", "", -1, false, err
	}

	var stdout, stderr []byte
	var exitCode int32
	var truncated bool

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return string(stdout), string(stderr), int(exitCode), truncated, err
		}
		switch msg.Type {
		case pb.OutputType_STDOUT:
			stdout = append(stdout, msg.Data...)
		case pb.OutputType_STDERR:
			stderr = append(stderr, msg.Data...)
		case pb.OutputType_EXIT:
			exitCode = msg.ExitCode
		case pb.OutputType_TRUNCATED:
			truncated = true
		}
	}

	return string(stdout), string(stderr), int(exitCode), truncated, nil
}

func (m *DevManager) Destroy(_ context.Context, sandboxID string) {
	m.mu.Lock()
	sb, ok := m.sandboxes[sandboxID]
	if ok {
		delete(m.sandboxes, sandboxID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}

	sb.cancel()
	sb.logMu.Lock()
	pgid := sb.appPgid
	sb.logMu.Unlock()
	if pgid > 0 {
		// The app server would otherwise outlive its sandbox, holding the port.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if sb.conn != nil {
		closeQuietly(sb.conn, "DevManager.Destroy")
	}
	if err := os.RemoveAll(sb.WorkDir); err != nil {
		m.log.Warn("sandbox_workdir_cleanup_failed", zap.String("id", sandboxID), zap.Error(err))
	}
	m.log.Info("sandbox_destroyed", zap.String("id", sandboxID))
}

// serveCatalog answers /v1/templates and /v1/templates/public with the same
// catalog nodes serve.
func serveCatalog(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, templates.Listed())
}

// errPathEscapesRoot lets handlers answer 400 for a confinement escape and
// 404 for a missing file.
var errPathEscapesRoot = errors.New("path escapes sandbox root")

var signalName = regexp.MustCompile(`^[A-Z0-9]{1,10}$`)

// resolveInWorkDir confines p to workDir. filepath.Join alone does not stop
// ".." from escaping, and a lexical check alone does not stop a symlink the
// sandbox created from pointing outside, so the real path is checked too.
// ponytail: check-then-use, so a link swapped in between can still race; os.Root per handler closes that.
func resolveInWorkDir(workDir, p string) (string, error) {
	full := filepath.Clean(filepath.Join(workDir, p))
	if !pathWithin(workDir, full) {
		return "", errPathEscapesRoot
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", err
	}
	real, err := realPath(full)
	if err != nil {
		return "", err
	}
	if !pathWithin(root, real) {
		return "", errPathEscapesRoot
	}
	return full, nil
}

func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath resolves symlinks in the longest existing prefix of p; what follows
// does not exist yet, so it cannot be a link. A dangling link is refused: a
// write through it would create its target, wherever that is.
func realPath(p string) (string, error) {
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", errPathEscapesRoot
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// WriteFile writes content to a path inside the sandbox workspace.
func (m *DevManager) WriteFile(sandboxID, filePath string, data []byte) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("sandbox not found")
	}
	fullPath, err := resolveInWorkDir(sb.WorkDir, filePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(fullPath, data, 0644)
}

// ReadFile reads a file from the sandbox workspace.
func (m *DevManager) ReadFile(sandboxID, filePath string) ([]byte, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox not found")
	}
	fullPath, err := resolveInWorkDir(sb.WorkDir, filePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(fullPath)
}

// ownedSandbox resolves the {id} in r for the caller. A sandbox that does not
// exist and a sandbox someone else owns are the same 404, so the id space
// cannot be probed. It writes the response itself; false means the caller is
// done.
func ownedSandbox(w http.ResponseWriter, r *http.Request, mgr *DevManager, ownerID func(*http.Request) string) (*DevSandbox, bool) {
	sb, ok := mgr.GetSandbox(chi.URLParam(r, "id"))
	if !ok || sb.UserID != ownerID(r) {
		http.Error(w, `{"error":"sandbox not found"}`, http.StatusNotFound)
		return nil, false
	}
	return sb, true
}

// registerFilesRoutes mounts the raw single-file read/write endpoints for the
// local runtime. Ownership is checked here (404 on mismatch, no existence
// oracle); path confinement lives in WriteFile/ReadFile.
func registerFilesRoutes(r chi.Router, mgr *DevManager, ownerID func(*http.Request) string) {
	r.Post("/v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
			return
		}
		filePath := r.URL.Query().Get("path")
		if filePath == "" {
			http.Error(w, `{"error":"path query param required"}`, http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
			return
		}
		if err := mgr.WriteFile(id, filePath, data); err != nil {
			status := http.StatusNotFound
			if errors.Is(err, errPathEscapesRoot) {
				status = http.StatusBadRequest
			}
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeBytes(w, []byte(`{"status":"ok"}`))
	})

	r.Get("/v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
			return
		}
		filePath := r.URL.Query().Get("path")
		if filePath == "" {
			http.Error(w, `{"error":"path query param required"}`, http.StatusBadRequest)
			return
		}
		data, err := mgr.ReadFile(id, filePath)
		if err != nil {
			status := http.StatusNotFound
			if errors.Is(err, errPathEscapesRoot) {
				status = http.StatusBadRequest
			}
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), status)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		writeBytes(w, data)
	})
}

// registerFilesystemRoutes mounts list/mkdir/move/remove/stat for the local
// runtime. Every handler checks ownership (404 on mismatch) and confines
// paths via resolveInWorkDir; remove refuses the sandbox root itself.
func registerFilesystemRoutes(r chi.Router, mgr *DevManager, ownerID func(*http.Request) string) {
	r.Get("/v1/sandboxes/{id}/filesystem/list", func(w http.ResponseWriter, r *http.Request) {
		dirPath := r.URL.Query().Get("path")
		if dirPath == "" {
			dirPath = "/"
		}
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		fullPath, err := resolveInWorkDir(sb.WorkDir, dirPath)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		result := []map[string]interface{}{}
		for _, e := range entries {
			info, ierr := e.Info()
			if ierr != nil {
				// Deleted between ReadDir and Info; ship the entry without size.
				info = nil
			}
			entry := map[string]interface{}{"name": e.Name(), "is_dir": e.IsDir()}
			if info != nil {
				entry["size"] = info.Size()
				entry["modified"] = info.ModTime().Format(time.RFC3339)
			}
			result = append(result, entry)
		}
		w.Header().Set("Content-Type", "application/json")
		// The fleet shape; the dashboard and SDKs read "entries".
		writeJSON(w, map[string]interface{}{"entries": result})
	})
	r.Post("/v1/sandboxes/{id}/filesystem/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		decodeJSONBody(r, &body)
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		fullPath, err := resolveInWorkDir(sb.WorkDir, body.Path)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeBytes(w, []byte(`{"status":"ok"}`))
	})
	r.Post("/v1/sandboxes/{id}/filesystem/move", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
		}
		decodeJSONBody(r, &body)
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		src, err := resolveInWorkDir(sb.WorkDir, body.Source)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		dst, err := resolveInWorkDir(sb.WorkDir, body.Destination)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		if err := os.Rename(src, dst); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeBytes(w, []byte(`{"status":"ok"}`))
	})
	r.Post("/v1/sandboxes/{id}/filesystem/remove", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		decodeJSONBody(r, &body)
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		full, err := resolveInWorkDir(sb.WorkDir, body.Path)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		if full == filepath.Clean(sb.WorkDir) {
			http.Error(w, `{"error":"refusing to remove sandbox root"}`, 400)
			return
		}
		if err := os.RemoveAll(full); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		w.WriteHeader(204)
	})
	r.Get("/v1/sandboxes/{id}/filesystem/stat", func(w http.ResponseWriter, r *http.Request) {
		filePath := r.URL.Query().Get("path")
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		fullPath, err := resolveInWorkDir(sb.WorkDir, filePath)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), 400)
			return
		}
		info, err := os.Stat(fullPath)
		if err != nil {
			http.Error(w, `{"error":"not found"}`, 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{
			"name": info.Name(), "size": info.Size(), "is_dir": info.IsDir(),
			"modified": info.ModTime().Format(time.RFC3339), "mode": info.Mode().String(),
		})
	})
}

// registerSandboxActionRoutes mounts exec, destroy, info, lifecycle, logs,
// metrics, process and history for the local runtime, each ownership-checked
// (404 on mismatch). preview-status stays in main because its fleet branch
// needs the proxy config.
func registerSandboxActionRoutes(r chi.Router, mgr *DevManager, ownerID func(*http.Request) string, execHistory *ExecHistory, usage *UsageTracker) {
	r.Post("/v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		var body struct {
			Cmd     string `json:"cmd"`
			Cwd     string `json:"cwd"`
			Timeout int    `json:"timeout"`
		}
		decodeJSONBody(r, &body)
		if body.Cmd == "" {
			http.Error(w, `{"error":"cmd required"}`, http.StatusBadRequest)
			return
		}
		timeout := time.Duration(body.Timeout) * time.Second
		if timeout == 0 {
			timeout = 30 * time.Second
		}

		execStart := time.Now()
		stdout, stderr, exitCode, truncated, err := mgr.Exec(r.Context(), id, body.Cmd, body.Cwd, timeout)
		execDur := int(time.Since(execStart).Milliseconds())
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusNotFound)
			return
		}
		execHistory.Record(id, body.Cmd, exitCode, execDur)

		// Mirror the command and output into the sandbox log so the Logs tab
		// shows terminal activity.
		sb.appendLog("$ " + body.Cmd)
		for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
			if line != "" {
				sb.appendLog(line)
			}
		}
		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			if line != "" {
				sb.appendLog(line)
			}
		}
		if exitCode != 0 {
			sb.appendLog(fmt.Sprintf("[exit %d]", exitCode))
		}

		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{
			"stdout": stdout, "stderr": stderr,
			"exit_code": exitCode, "truncated": truncated,
		})
	})

	r.Delete("/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// Deleting a missing sandbox is a 204; deleting someone else's is a 404.
		if sb, ok := mgr.GetSandbox(id); ok {
			if sb.UserID != ownerID(r) {
				http.Error(w, `{"error":"sandbox not found"}`, 404)
				return
			}
			dur := time.Since(sb.CreatedAt).Seconds()
			usage.RecordDestroy(dur, 2, 512) // default 2 CPU, 512MB
		}
		mgr.Destroy(r.Context(), id)
		w.WriteHeader(http.StatusNoContent)
	})

	r.Get("/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		base := "http://" + r.Host
		status, code := sb.statusSnapshot()
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{
			"id": sb.ID, "status": status, "exit_code": code, "template": sb.Template,
			"boot_type": sb.BootType, "boot_ms": sb.BootMs,
			"workspace":  sb.WorkDir,
			"url":        base + "/sandbox/" + sb.ID + "/",
			"api_exec":   base + "/v1/sandboxes/" + sb.ID + "/exec",
			"api_files":  base + "/v1/sandboxes/" + sb.ID + "/files",
			"created_at": sb.CreatedAt, "expires_at": sb.ExpiresAt,
		})
	})

	// The local runtime cannot pause: answering "paused" while the processes
	// keep running told every client something false.
	for _, action := range []string{"pause", "resume"} {
		r.Post("/v1/sandboxes/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
				return
			}
			http.Error(w, `{"error":"`+action+` requires fleet mode: the local runtime runs sandboxes as plain processes"}`, http.StatusNotImplemented)
		})
	}
	r.Post("/v1/sandboxes/{id}/timeout", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		// "timeout" is the pre-SDK-fix field name, still accepted from old clients.
		var body struct {
			Seconds int `json:"seconds"`
			Timeout int `json:"timeout"`
		}
		decodeJSONBody(r, &body)
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		seconds := body.Seconds
		if seconds == 0 {
			seconds = body.Timeout
		}
		if seconds == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": `"seconds" is required: a positive TTL from now`})
			return
		}
		if seconds > 0 {
			sb.ExpiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"sandbox_id": id, "expires_at": sb.ExpiresAt})
	})
	r.Post("/v1/sandboxes/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		// Extends, never shortens: a sandbox created with an hour left keeps it.
		if next := time.Now().Add(5 * time.Minute); next.After(sb.ExpiresAt) {
			sb.ExpiresAt = next
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"sandbox_id": id, "expires_at": sb.ExpiresAt})
	})
	r.Get("/v1/sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		lines := sb.logsSnapshot()
		note := ""
		if len(lines) == 0 {
			note = "No process output yet. Deploy a process or run a command in the terminal."
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{"lines": lines, "note": note})
	})
	r.Get("/v1/sandboxes/{id}/metrics", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		sb, ok := ownedSandbox(w, r, mgr, ownerID)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]interface{}{
			"sandbox_id": id, "cpu_count": 2, "memory_mb": 512,
			"uptime_seconds": time.Since(sb.CreatedAt).Seconds(),
			"exec_count":     len(execHistory.BySandbox(id)),
		})
	})

	r.Get("/v1/sandboxes/{id}/process/list", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
			return
		}
		stdout, _, _, _, err := mgr.Exec(context.Background(), id, "ps aux --no-headers 2>/dev/null || ps -e -o pid,comm 2>/dev/null | head -20", "", 5*time.Second)
		if err != nil {
			log.Printf("process/list: exec failed for %s: %v", id, err)
		}
		var procs []map[string]string
		for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
			if line != "" {
				procs = append(procs, map[string]string{"raw": line})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, procs)
	})
	r.Post("/v1/sandboxes/{id}/process/signal", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body struct {
			PID    int    `json:"pid"`
			Signal string `json:"signal"`
		}
		decodeJSONBody(r, &body)
		if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
			return
		}
		sig := body.Signal
		if sig == "" {
			sig = "SIGTERM"
		}
		// It is interpolated into a shell command.
		if !signalName.MatchString(sig) {
			http.Error(w, `{"error":"signal must be a name like SIGTERM or a number"}`, http.StatusBadRequest)
			return
		}
		if _, _, _, _, err := mgr.Exec(context.Background(), id, fmt.Sprintf("kill -%s %d 2>/dev/null", sig, body.PID), "", 5*time.Second); err != nil {
			log.Printf("process/signal: exec failed for %s: %v", id, err)
		}
		w.Header().Set("Content-Type", "application/json")
		writeBytes(w, []byte(`{"status":"ok"}`))
	})

	r.Get("/v1/sandboxes/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, ok := ownedSandbox(w, r, mgr, ownerID); !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, execHistory.BySandbox(id))
	})
}

// GetSandbox returns sandbox info.
func (m *DevManager) GetSandbox(sandboxID string) (*DevSandbox, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[sandboxID]
	return sb, ok
}

// StartProcess runs a long-lived process directly (not through envd).
// Replaces the requested port with the sandbox's assigned unique port.
func (m *DevManager) StartProcess(sandboxID, command string, requestedPort int) (int, int, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return 0, 0, fmt.Errorf("sandbox not found")
	}

	actualPort := sb.Port
	if requestedPort > 0 && requestedPort != actualPort {
		rp := fmt.Sprintf("%d", requestedPort)
		ap := fmt.Sprintf("%d", actualPort)
		for _, pat := range []string{":%s", "port=%s", "port %s", "PORT=%s", "port=%s", "listen(%s", " %s)", "=%s\n", " %s,"} {
			command = strings.ReplaceAll(command, fmt.Sprintf(pat, rp), fmt.Sprintf(pat, ap))
		}
		command = strings.ReplaceAll(command, rp, ap)
	}

	// Not CommandContext: the server must outlive the request that started it.
	cmd := exec.Command("/bin/sh", "-c", command) //nolint:noctx // see above: the process outlives the request by design
	cmd.Dir = sb.WorkDir
	// Create-time env first, PORT last, matching the fleet runtimes' precedence.
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", actualPort))
	cmd.Env = append(cmd.Env, sb.BootEnv...)
	// Its own group, so Destroy can stop the server and whatever it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	sb.setStatus("running", 0)
	sb.markAppStarted()
	sb.logMu.Lock()
	sb.appPgid = cmd.Process.Pid
	sb.logMu.Unlock()
	sb.appendLog("$ " + command)

	stream := func(rc io.ReadCloser) {
		scan := bufio.NewScanner(rc)
		scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scan.Scan() {
			sb.appendLog(scan.Text())
		}
	}
	go stream(stdout)
	go stream(stderr)

	go func() {
		werr := cmd.Wait()
		switch werr {
		case nil:
			sb.setStatus("exited", 0)
			sb.appendLog("[process exited: code 0]")
		default:
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				sb.setStatus("crashed", ee.ExitCode())
				sb.appendLog(fmt.Sprintf("[process crashed: exit code %d]", ee.ExitCode()))
			} else {
				sb.setStatus("crashed", -1)
				sb.appendLog("[process error: " + werr.Error() + "]")
			}
		}
	}()
	return cmd.Process.Pid, actualPort, nil
}

// List returns owner's sandboxes; every other tenant's are invisible.
func (m *DevManager) List(owner string) []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		if sb.UserID != owner {
			continue
		}
		status, code := sb.statusSnapshot()
		out = append(out, map[string]interface{}{
			"id": sb.ID, "template": sb.Template, "user_id": sb.UserID,
			"boot_type": sb.BootType, "boot_ms": sb.BootMs,
			"status": status, "exit_code": code,
			"vm_ip":      fmt.Sprintf("localhost:%d", sb.Port),
			"created_at": sb.CreatedAt, "expires_at": sb.ExpiresAt,
		})
	}
	return out
}

func (m *DevManager) Stats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	perTemplate := make(map[string]int)
	for _, sb := range m.sandboxes {
		perTemplate[sb.Template]++
	}

	poolStats := make(map[string]interface{})
	totalCreates := m.createCount.Load()
	for _, tpl := range m.templates {
		count := perTemplate[tpl]
		poolStats[tpl] = map[string]interface{}{
			"template": tpl, "ready": m.poolSize, "capacity": m.poolSize,
			"active": count, "hits": totalCreates, "misses": int64(0), "miss_rate": 0.0,
		}
	}

	return map[string]interface{}{
		"active_count": len(m.sandboxes), "max_active": m.maxActive,
		"disk_usage": 0.0, "disk_pressure": false,
		"pool_stats":     poolStats,
		"rate_limit_rps": 5.0, "rate_limit_burst": 10,
		"max_concurrent": m.maxConcurrent, "pool_size": m.poolSize,
		"templates": m.templates,
	}
}

func (m *DevManager) Config() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]interface{}{
		"pool_size": m.poolSize, "max_active": m.maxActive,
		"max_concurrent": m.maxConcurrent, "rate_limit_rps": 5.0,
		"rate_limit_burst": 10, "templates": m.templates,
	}
}

// syncLog flushes the logger, ignoring the error: syncing stderr on macOS
// fails even though the write succeeded. Call it before log.Fatal, which
// skips defers.
func syncLog(l *zap.Logger) {
	if err := l.Sync(); err != nil {
		_ = err
	}
}

// startLocalEnvd starts the envd the in-process runtime talks to. A portal in
// front of a fleet has no use for one, and envd listens on every interface with
// a web terminal, so it starts nothing. Checked before anything else starts:
// log.Fatal skips deferred cleanup.
func startLocalEnvd(log *zap.Logger, bin string, proxyMode bool) (*exec.Cmd, string) {
	if proxyMode {
		return nil, ""
	}
	if _, err := os.Stat(bin); err != nil {
		log.Fatal("envd binary not found",
			zap.String("path", bin),
			zap.String("fix", "go build -o "+bin+" ./cmd/envd/"),
			zap.String("override", "set ENVD_BIN=<path> to use a different location"),
			zap.Error(err),
		)
	}
	log.Info("starting_envd", zap.String("bin", bin))
	// Not CommandContext: envd's lifetime is managed at shutdown, not by a context.
	cmd := exec.Command(bin) //nolint:noctx // see above
	token, hash := sbtoken.Mint()
	cmd.Env = append(os.Environ(), "ENVD_TOKEN_HASH="+hash)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatal("failed to start envd", zap.Error(err))
	}
	return cmd, token
}

// stopEnvd kills and reaps the shared envd child. Early-exit paths call it
// directly because log.Fatal skips defers.
func stopEnvd(log *zap.Logger, envdCmd *exec.Cmd) {
	if envdCmd == nil {
		return
	}
	if err := envdCmd.Process.Kill(); err != nil {
		log.Warn("envd_kill_failed", zap.Error(err))
	}
	if err := envdCmd.Wait(); err != nil {
		log.Warn("envd_wait_failed", zap.Error(err))
	}
}

// mustInitWebUI builds the customer console or exits, stopping envd first
// because os.Exit skips main's deferred cleanup.
func mustInitWebUI(log *zap.Logger, envdCmd *exec.Cmd, devKey, uiRuntime, portalSandboxDomain string) *webui.Server {
	customerUI, uiErr := webui.New(devKey, uiRuntime, "user", false, portalSandboxDomain)
	if uiErr != nil {
		log.Error("webui init failed", zap.Error(uiErr))
		stopEnvd(log, envdCmd)
		syncLog(log)
		os.Exit(1)
	}
	return customerUI
}

func main() {
	baseLog, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("zap.NewProduction: %v", err)
	}
	log, logBroadcaster := newDashLogger(baseLog)
	legacyDomains, err := portalLegacyDomains(os.Getenv("PORTAL_LEGACY_SANDBOX_DOMAINS"), os.Getenv("PORTAL_ORCHESTRATOR_URL"))
	if err != nil {
		log.Fatal("invalid preview domain settings", zap.Error(err))
	}

	listenAddr := ":" + config.Str("PORT", "8080")
	maxSandboxes := 50
	if v := os.Getenv("MAX_SANDBOXES"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxSandboxes); err != nil {
			log.Warn("invalid MAX_SANDBOXES, using default", zap.String("value", v), zap.Int("default", maxSandboxes))
		}
	}

	orchURL := os.Getenv("PORTAL_ORCHESTRATOR_URL")
	orchKey := os.Getenv("PORTAL_ORCHESTRATOR_KEY")
	// The in-process runtime shells out on this host, so it must be opted
	// into explicitly; see execmode.go.
	execWhere := sandboxExecMode(orchURL, orchKey, os.Getenv(PortalLocalRuntimeEnv))
	proxyMode := execWhere == execProxy

	envdBin := config.Str("ENVD_BIN", "./bin/envd-local")
	envdCmd, envdToken := startLocalEnvd(log, envdBin, proxyMode)
	// Nothing above this point may defer: log.Fatal skips defers.
	defer syncLog(log)
	defer stopEnvd(log, envdCmd)
	// Bounds boot-time store calls so startup cannot hang on an unreachable database.
	bootCtx, stopBoot := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBoot()

	if envdCmd != nil {
		// Give envd a moment to come up.
		time.Sleep(1 * time.Second)
		log.Info("envd_started", zap.Int("pid", envdCmd.Process.Pid))
	}

	mgr := NewDevManager(envdBin, log)
	mgr.envdToken = envdToken
	mgr.maxActive = maxSandboxes
	// Dashboard-minted keys (sf_), persisted hash-at-rest.
	apiKeysPath := os.Getenv("PORTAL_API_KEYS_FILE")
	if apiKeysPath == "" {
		apiKeysPath = "api_keys.json"
	}
	keyStore := LoadAPIKeys(apiKeysPath)
	usage := NewUsageTracker()
	execHistory := NewExecHistory()

	go func() {
		for range time.NewTicker(1 * time.Second).C {
			mgr.mu.RLock()
			count := len(mgr.sandboxes)
			mgr.mu.RUnlock()
			usage.Snapshot(count)
		}
	}()

	// PORTAL_API_KEY gates the portal in production; unset (local make dev)
	// means a random key and anonymous access.
	requireAuth := os.Getenv("PORTAL_API_KEY") != ""
	devKey := config.Str("PORTAL_API_KEY", "sf_"+randomHex(24))
	keyStore.seedDevKey(devKey)

	// With an orchestrator configured, fleet paths are proxied to it; the
	// portal enforces tenancy over the single orchestrator org.
	// Preview tokens are HMAC'd with PREVIEW_SECRET, falling back to the
	// orchestrator key. Empty means preview gating is off.
	if s := os.Getenv("PREVIEW_SECRET"); s != "" {
		previewSecret = []byte(s)
	} else if orchKey != "" {
		previewSecret = previewtoken.BrowserKey(orchKey)
	}
	// Secrets vault: PORTAL_VAULT_KEY, else the orchestrator key, else the dev
	// key. Rotating the material makes stored ciphertexts undecryptable.
	vaultPath := os.Getenv("PORTAL_VAULT_FILE")
	if vaultPath == "" {
		// A relative default lives in the working directory, which is
		// ephemeral on container platforms.
		if d := os.Getenv("PORTAL_DATA"); d != "" {
			if err := os.MkdirAll(d, 0o700); err != nil {
				log.Warn("vault_data_dir_create_failed", zap.String("path", d), zap.Error(err))
			}
			vaultPath = filepath.Join(d, "vault.json")
		} else {
			vaultPath = "vault.json"
			log.Warn("vault_ephemeral_path",
				zap.String("path", vaultPath),
				zap.String("impact", "provider keys are in the working directory and are LOST ON REDEPLOY"),
				zap.String("fix", "set PORTAL_DATA or PORTAL_VAULT_FILE to a persistent volume"))
		}
	}

	// Projects share SANDBOXD_DATABASE_URL when set, else a local SQLite file.
	// Not fatal: registerProjectRoutes mounts nothing for a nil store.
	projectDSN := os.Getenv("SANDBOXD_DATABASE_URL")
	if projectDSN == "" {
		projectDSN = os.Getenv("PORTAL_PROJECTS_DB")
	}
	if projectDSN == "" {
		projectDSN = "projects.db"
	}
	projectStore, projErr := pdstore.Open(bootCtx, projectDSN)
	if projErr != nil {
		log.Warn("projects store unavailable — Projects disabled, sandboxes unaffected", zap.Error(projErr))
		projectStore = nil
	}
	vaultKeyMaterial := os.Getenv("PORTAL_VAULT_KEY")
	if vaultKeyMaterial == "" {
		// Coupling the vault to the ops credential means rotating it makes
		// every stored key undecryptable, so warn loudly.
		vaultKeyMaterial = orchKey
		if vaultKeyMaterial != "" {
			log.Warn("vault_key_coupled_to_ops_credential",
				zap.String("impact", "rotating ORCHESTRATOR_API_KEY makes every stored provider key undecryptable"),
				zap.String("fix", "set PORTAL_VAULT_KEY to decouple customer data from the ops key"))
		}
	}
	if vaultKeyMaterial == "" {
		vaultKeyMaterial = devKey
	}
	vault := LoadVault(vaultPath, vaultKeyMaterial)

	// The UI's environment badge: "cloud" on the fleet, "local" in-process.
	uiRuntime := "local"
	if proxyMode {
		uiRuntime = "cloud"
	}

	// Customer console: the React SPA in "user" mode from the embedded dist.
	customerUI := mustInitWebUI(log, envdCmd, devKey, uiRuntime, portalSandboxDomain)
	uiMux := http.NewServeMux()
	customerUI.Register(uiMux)

	ownerID := func(r *http.Request) string {
		// The API key is the identity; there is no hosted identity provider.
		tok := presentedToken(r)
		if owner := keyStore.OwnerOf(tok); owner != "" {
			return owner // dashboard-minted key
		}
		if tok != "" {
			return apiKeyOwner(tok) // unknown or legacy token: isolated k:* tenancy
		}
		return "anon"
	}

	// sandboxProxy short-circuits fleet paths to the orchestrator in proxy
	// mode. Tenancy is server-authoritative via X-Org-Override.
	sandboxProxy := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A half-configured deployment must not fall through to the
			// in-process runtime, which runs customer commands as this
			// process's user. 501 is not a status anything retries.
			if execWhere == execRefuse && strings.HasPrefix(r.URL.Path, "/v1/sandboxes") {
				http.Error(w, `{"error":"sandboxes require fleet mode: set PORTAL_ORCHESTRATOR_URL and PORTAL_ORCHESTRATOR_KEY, or PORTAL_LOCAL_RUNTIME=1 for local development"}`, http.StatusNotImplemented)
				return
			}
			if !proxyMode || !isFleetPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// Streaming exec must not go through the buffering proxy.
			if strings.HasSuffix(r.URL.Path, "/exec/stream") {
				streamToOrchestrator(w, r, orchURL, orchKey, ownerID(r))
				return
			}
			owner := ownerID(r)
			r.Header.Set("X-Org-Override", owner)
			rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes"), "/")
			id := ""
			if rest != "" {
				id = strings.SplitN(rest, "/", 2)[0]
			}
			// On create, resolve secret_refs; remember the first port for a
			// preview URL in the response.
			previewPort := 0
			if r.Method == "POST" && id == "" {
				if raw, err := io.ReadAll(r.Body); err == nil {
					closeQuietly(r.Body, "sandboxes create")
					// secret_refs resolve from the caller's vault and are
					// stripped; nodes never see the vault.
					resolved, rerr := resolveSecretRefs(raw, vault, owner)
					if rerr != nil {
						http.Error(w, fmt.Sprintf(`{"error":%q}`, rerr.Error()), http.StatusBadRequest)
						return
					}
					previewPort = firstPort(resolved)
					r.Body = io.NopCloser(bytes.NewReader(resolved))
				}
			}
			body, status, ctype := proxyToOrchestrator(r, orchURL, orchKey, ownerID(r))
			if status == 0 {
				http.Error(w, `{"error":"orchestrator unreachable"}`, http.StatusBadGateway)
				return
			}
			if status < 300 {
				// Customers see portal-branded preview URLs, never node addresses.
				body = rebrandBody(r, body)
			}
			if r.Method == "POST" && id == "" && status < 300 { // create
				var obj map[string]any
				if json.Unmarshal(body, &obj) == nil {
					if sid, _ := obj["id"].(string); sid != "" {
						// Add a preview URL for the app port when the node gave none.
						if _, has := obj["url"].(string); (!has || obj["url"] == "") && previewPort > 0 {
							obj["url"] = previewURL(r, sid, previewPort)
							if nb, err := json.Marshal(obj); err == nil {
								body = nb
							}
						}
					}
				}
			}
			if ctype != "" {
				w.Header().Set("Content-Type", ctype)
			}
			w.WriteHeader(status)
			writeBytes(w, body)
		})
	}
	// "refuse" in a production log means the fleet credentials did not arrive.
	log.Info("sandbox_exec_mode", zap.String("mode", execWhere.String()), zap.String("orchestrator", orchURL))
	if execWhere == execLocal {
		log.Warn("sandbox_local_runtime_enabled",
			zap.String("detail", "sandbox commands run on THIS host, unisolated — "+PortalLocalRuntimeEnv+" must never be set in production"))
	}

	apiAuth := newAPIAuth(keyStore, requireAuth)

	r := chi.NewRouter()
	r.Use(chimid.Recoverer)
	// JSON only: streams, downloads and previews pass through untouched.
	r.Use(chimid.Compress(5, "application/json"))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
			// API responses are live state; never let the browser cache them.
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				w.Header().Set("Cache-Control", "no-store")
			}
			if r.Method == "OPTIONS" {
				w.WriteHeader(204)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	statsFeed := NewBroadcaster()
	go func() {
		for range time.NewTicker(1 * time.Second).C {
			stats := mgr.Stats()
			uStats := usage.Stats()
			stats["usage"] = uStats
			stats["start_rate"] = uStats["start_rate"]
			stats["peak_concurrent"] = uStats["peak_concurrent"]
			data, err := json.Marshal(stats)
			if err != nil {
				continue
			}
			statsFeed.Send(data)
		}
	}()

	// Public site, served from the same webui embed as the console.
	r.Handle("/", uiMux)
	r.Handle("/docs", uiMux)
	r.Handle("/guides", uiMux)
	for _, p := range webui.PublicPages {
		r.Handle("/"+p, uiMux)
	}
	// The signed-out catalog fallback, as the orchestrator serves it.
	r.Get("/v1/templates/public", serveCatalog)
	r.Handle("/og.png", uiMux)
	// A prefix, unlike the enumerated pages, so a new image in web/public
	// does not 404 on the portal.
	r.Handle("/img/*", uiMux)
	// CLI distribution: install script and binaries from PILLDROPS_DL_DIR.
	r.Handle("/install.sh", uiMux)
	r.Handle("/dl/*", uiMux)
	r.Handle("/llms.txt", uiMux)
	// MCP tool calls re-enter this router at /v1 with the caller's bearer
	// key, so auth and limits apply as for any other client.
	r.Handle("/mcp", &mcp.HTTPServer{API: r})
	r.Get("/openapi.json", apispec.Serve)

	// Public, for load balancers.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeBytes(w, []byte(`{"status":"ok"}`))
	})

	// Public contact form. Every submission is logged; FEEDBACK_WEBHOOK_URL
	// also gets it (Discord/Slack-compatible payload).
	r.Post("/feedback", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in struct {
			Name    string `json:"name"`
			Email   string `json:"email"`
			Kind    string `json:"kind"`
			Message string `json:"message"`
			Website string `json:"website"` // honeypot; real users leave this empty
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 32<<10)).Decode(&in); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		if in.Website != "" { // a bot filled the honeypot: drop it, pretend success
			writeBytes(w, []byte(`{"ok":true}`))
			return
		}
		msg := strings.TrimSpace(in.Message)
		if msg == "" || len(msg) > 8000 || len(in.Email) > 254 || len(in.Name) > 200 {
			http.Error(w, `{"error":"please include a message"}`, http.StatusBadRequest)
			return
		}
		kind := strings.TrimSpace(in.Kind)
		if kind == "" {
			kind = "feedback"
		}
		ip := req.Header.Get("X-Forwarded-For")
		if ip == "" {
			ip = req.RemoteAddr
		}
		log.Info("feedback_received",
			zap.String("kind", kind),
			zap.String("name", in.Name),
			zap.String("email", in.Email),
			zap.String("ip", ip),
			zap.String("message", msg),
		)
		if hook := os.Getenv("FEEDBACK_WEBHOOK_URL"); hook != "" {
			text := fmt.Sprintf("📨 New %s\nfrom: %s <%s>\n\n%s", kind, in.Name, in.Email, msg)
			go func() {
				payload, err := json.Marshal(map[string]string{"content": text, "text": text})
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				hr, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(payload))
				if err != nil {
					return
				}
				hr.Header.Set("Content-Type", "application/json")
				if resp, err := http.DefaultClient.Do(hr); err == nil { //nolint:bodyclose // closed by closeQuietly on the next line; bodyclose can't see through the helper
					closeQuietly(resp.Body, "feedback webhook")
				}
			}()
		}
		// Best-effort forward to the orchestrator's feedback inbox for the admin console.
		if orchURL != "" && orchKey != "" {
			fwd := map[string]string{"name": in.Name, "email": in.Email, "kind": kind, "message": msg, "source": "contact-form", "ip": ip}
			go func() {
				payload, err := json.Marshal(fwd)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				fr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(orchURL, "/")+"/v1/feedback", bytes.NewReader(payload))
				if err != nil {
					return
				}
				fr.Header.Set("Content-Type", "application/json")
				fr.Header.Set("Authorization", "Bearer "+orchKey)
				if resp, err := http.DefaultClient.Do(fr); err == nil { //nolint:bodyclose // closed by closeQuietly on the next line
					closeQuietly(resp.Body, "feedback orchestrator forward")
				}
			}()
		}
		writeBytes(w, []byte(`{"ok":true}`))
	})

	// Public waitlist. No local copy: the synchronous forward to the
	// orchestrator's store is the persistence.
	r.Post("/waitlist", newWaitlistHandler(orchURL, orchKey, os.Getenv("FEEDBACK_WEBHOOK_URL")))

	// Console shell and assets, scoped so /dashboard/api/* below does not
	// collide with a wildcard.
	r.Handle("/dashboard", uiMux)
	r.Handle("/dashboard/assets/*", uiMux)
	// The webui's key-to-cookie login must be mounted explicitly or it 404s.
	r.Handle("/dashboard/login", uiMux)

	// Gated behind apiAuth: /dashboard/api/logs streams the portal's own server logs.
	r.Group(func(r chi.Router) {
		r.Use(apiAuth)

		r.Get("/dashboard/api/stats", func(w http.ResponseWriter, _ *http.Request) {
			stats := mgr.Stats()
			uStats := usage.Stats()
			stats["usage"] = uStats
			stats["start_rate"] = uStats["start_rate"]
			stats["peak_concurrent"] = uStats["peak_concurrent"]
			stats["total_starts"] = uStats["total_starts"]
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, stats)
		})

		r.Get("/dashboard/api/sandboxes", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			// In proxy mode read the customer's sandboxes from the orchestrator
			// and map the fleet's "state" onto the dashboard's "status".
			if proxyMode {
				rr := r.Clone(r.Context())
				rr.Method = "GET"
				rr.URL.Path = "/v1/sandboxes"
				rr.Header.Set("X-Org-Override", ownerID(r))
				body, status, _ := proxyToOrchestrator(rr, orchURL, orchKey, ownerID(r))
				if status == 0 {
					writeBytes(w, []byte("[]"))
					return
				}
				var obj struct {
					Sandboxes []map[string]any `json:"sandboxes"`
				}
				if err := json.Unmarshal(body, &obj); err != nil {
					// A malformed response shows no rows rather than an error.
					log.Warn("dashboard sandboxes: could not parse orchestrator response", zap.Error(err))
				}
				rows := make([]map[string]any, 0, len(obj.Sandboxes))
				for _, s := range obj.Sandboxes {
					if st, _ := s["status"].(string); st == "" {
						if v, ok := s["state"].(string); ok {
							s["status"] = v
						}
					}
					if _, ok := s["boot_type"]; !ok {
						s["boot_type"] = ""
					}
					if _, ok := s["boot_ms"]; !ok {
						s["boot_ms"] = 0
					}
					rows = append(rows, s)
				}
				writeJSON(w, rows)
				return
			}
			writeJSON(w, mgr.List(ownerID(r)))
		})

		r.Delete("/dashboard/api/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
			id := chi.URLParam(r, "id")
			// In proxy mode the sandbox lives on the fleet.
			if proxyMode {
				rr := r.Clone(r.Context())
				rr.Method = "DELETE"
				rr.URL.Path = "/v1/sandboxes/" + id
				rr.Header.Set("X-Org-Override", ownerID(r))
				_, status, _ := proxyToOrchestrator(rr, orchURL, orchKey, ownerID(r))
				if status == 0 {
					http.Error(w, `{"error":"orchestrator unreachable"}`, http.StatusBadGateway)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if sb, ok := mgr.GetSandbox(id); ok {
				// Same rule as DELETE /v1/sandboxes/{id}: someone else's is a 404.
				if sb.UserID != ownerID(r) {
					http.Error(w, `{"error":"sandbox not found"}`, http.StatusNotFound)
					return
				}
				dur := time.Since(sb.CreatedAt).Seconds()
				usage.RecordDestroy(dur, 2, 512)
			}
			mgr.Destroy(r.Context(), id)
			w.WriteHeader(http.StatusNoContent)
		})

		r.Get("/dashboard/api/config", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, mgr.Config())
		})

		r.Put("/dashboard/api/config", func(w http.ResponseWriter, r *http.Request) {
			// Dev mode: just acknowledge
			w.Header().Set("Content-Type", "application/json")
			writeBytes(w, []byte(`{"status":"applied"}`))
		})

		r.Get("/dashboard/api/events", func(w http.ResponseWriter, r *http.Request) {
			f, ok := w.(http.Flusher)
			if !ok {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			f.Flush()
			ch := statsFeed.Subscribe()
			defer statsFeed.Unsubscribe(ch)
			for {
				select {
				case data := <-ch:
					writeFprintf(w, "event: stats\ndata: %s\n\n", data)
					f.Flush()
				case <-r.Context().Done():
					return
				}
			}
		})

		r.Get("/dashboard/api/logs", func(w http.ResponseWriter, r *http.Request) {
			f, ok := w.(http.Flusher)
			if !ok {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			f.Flush()
			ch := logBroadcaster.Subscribe()
			defer logBroadcaster.Unsubscribe(ch)
			for {
				select {
				case data, ok := <-ch:
					if !ok {
						return
					}
					writeFprintf(w, "event: log\ndata: %s\n\n", data)
					f.Flush()
				case <-r.Context().Done():
					return
				}
			}
		})
	})

	r.Group(func(r chi.Router) {
		r.Use(apiAuth)
		r.Use(sandboxProxy) // fleet paths go to the orchestrator in proxy mode

		r.Post("/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Template string `json:"template"`
				Timeout  int    `json:"timeout"`
				// Optional: deploy a full app on create
				Files []struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				} `json:"files"`
				Setup []string `json:"setup"` // e.g. ["pip install flask"]
				Start string   `json:"start"` // e.g. "python3 app.py"
				Port  int      `json:"port"`  // e.g. 8000, the port the app serves
				// Env is create-time environment, "NAME=VALUE", same shape as the fleet API.
				Env []string `json:"env"`
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
				return
			}
			if len(bytes.TrimSpace(raw)) > 0 && !json.Valid(raw) {
				http.Error(w, `{"error":"request body is not valid JSON"}`, http.StatusBadRequest)
				return
			}
			// secret_refs and auto-inject vault entries become env, as on the fleet.
			if raw, err = resolveSecretRefs(raw, vault, ownerID(r)); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &body); err != nil {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
					return
				}
			}
			if body.Template == "" {
				body.Template = "python-3.11"
			}
			timeout := time.Duration(body.Timeout) * time.Second
			if timeout == 0 {
				timeout = 5 * time.Minute
			}

			sb, err := mgr.Create(r.Context(), body.Template, ownerID(r), timeout, body.Env)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusServiceUnavailable)
				return
			}
			usage.RecordStart()

			base := "http://" + r.Host
			resp := map[string]interface{}{
				"id": sb.ID, "status": "running", "template": sb.Template,
				"boot_type": sb.BootType, "boot_ms": sb.BootMs,
				"url":        base + "/sandbox/" + sb.ID + "/",
				"created_at": sb.CreatedAt, "expires_at": sb.ExpiresAt,
			}

			if len(body.Files) > 0 || len(body.Setup) > 0 || body.Start != "" {
				var setupLog []string

				for _, f := range body.Files {
					if err := mgr.WriteFile(sb.ID, f.Path, []byte(f.Content)); err != nil {
						setupLog = append(setupLog, "write "+f.Path+": "+err.Error())
					} else {
						setupLog = append(setupLog, "wrote "+f.Path)
					}
				}

				for _, cmd := range body.Setup {
					r, _, exitCode, _, err := mgr.Exec(context.Background(), sb.ID, cmd, "", 120*time.Second)
					switch {
					case err != nil:
						setupLog = append(setupLog, "setup error: "+err.Error())
					case exitCode != 0:
						setupLog = append(setupLog, "setup '"+cmd+"' exit="+fmt.Sprintf("%d", exitCode))
					default:
						setupLog = append(setupLog, "setup '"+cmd+"' ok")
					}
					_ = r
				}

				// A direct background process, so envd's exec timeout does not kill it.
				if body.Start != "" {
					reqPort := body.Port
					if reqPort == 0 {
						reqPort = 8000
					}
					pid, actualPort, err := mgr.StartProcess(sb.ID, body.Start, reqPort)
					if err != nil {
						setupLog = append(setupLog, "start error: "+err.Error())
					} else {
						setupLog = append(setupLog, fmt.Sprintf("started: %s (pid=%d, port=%d)", body.Start, pid, actualPort))
						time.Sleep(1 * time.Second)
						resp["app_url"] = fmt.Sprintf("%s/preview/%s/%d/", base, sb.ID, actualPort)
						resp["port"] = actualPort
					}
				}

				resp["setup_log"] = setupLog
				if body.Port > 0 {
					resp["app_url"] = fmt.Sprintf("%s/preview/%s/%d/", base, sb.ID, body.Port)
				}
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, resp)
		})

		// fleetProxy forwards a request to the orchestrator scoped to the caller,
		// or serves fallback in local mode. proxyToOrchestrator asserts the owner
		// itself, so the inbound X-Org-Override is never relayed.
		fleetProxy := func(w http.ResponseWriter, r *http.Request, fallback func(w http.ResponseWriter)) {
			if !proxyMode {
				w.Header().Set("Content-Type", "application/json")
				fallback(w)
				return
			}
			body, status, ctype := proxyToOrchestrator(r, orchURL, orchKey, ownerID(r))
			if status == 0 {
				http.Error(w, `{"error":"orchestrator unreachable"}`, http.StatusBadGateway)
				return
			}
			if ctype != "" {
				w.Header().Set("Content-Type", ctype)
			}
			w.WriteHeader(status)
			writeBytes(w, body)
		}

		// Explicit routes are required: chi only runs sandboxProxy for matched
		// patterns, so a fleetPaths prefix alone still 404s.
		fleetPassthrough := func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { answerWithoutFleet(w, r) })
		}
		registerFleetPassthrough(r, fleetPassthrough)

		// Long-lived server on an existing sandbox; explicit route so the proxy engages.
		r.Post("/v1/sandboxes/{id}/start", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				http.Error(w, `{"error":"start requires fleet mode"}`, http.StatusNotImplemented)
			})
		})

		// Unbuffered streaming reverse proxy so agents see output live;
		// FlushInterval -1 flushes every frame.
		r.Post("/v1/sandboxes/{id}/exec/stream", func(w http.ResponseWriter, r *http.Request) {
			base, err := url.Parse(orchURL)
			if orchURL == "" || err != nil {
				http.Error(w, `{"error":"streaming exec requires the fleet backend"}`, http.StatusServiceUnavailable)
				return
			}
			owner, keyID := ownerID(r), requestKeyID(r)
			proxy := httputil.NewSingleHostReverseProxy(base)
			proxy.FlushInterval = -1
			inner := proxy.Director
			proxy.Director = func(req *http.Request) {
				inner(req)
				req.Host = base.Host
				req.Header.Set("Authorization", "Bearer "+orchKey)
				req.Header.Set("X-Org-Override", owner)
				// Set or delete, never left alone: this proxy carries the
				// client's own headers through, so a forged key id would ride
				// along on the orchestrator's credential.
				req.Header.Del(keyIDHeader)
				if keyID != "" {
					req.Header.Set(keyIDHeader, keyID)
				}
			}
			proxy.ServeHTTP(w, r)
		})

		// Rename; explicit route so the proxy engages. Local mode has no
		// persisted name to set.
		r.Patch("/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				http.Error(w, `{"error":"rename requires fleet mode"}`, http.StatusNotImplemented)
			})
		})

		// Background deploy progress (async_setup); explicit route so the proxy engages.
		r.Get("/v1/sandboxes/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				// Local mode runs setup inline. "unknown" rather than an error,
				// or the poller retries to its deadline.
				writeBytes(w, []byte(`{"state":"unknown","steps":[],"log":[]}`))
			})
		})

		// Preview readiness: a server-side probe of whether the app answers,
		// same-origin so the dashboard can read the real status.
		r.Get("/v1/sandboxes/{id}/preview-status", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				// Local mode probes directly: this runtime assigned the port and
				// reaps the process, so it knows more than a fleet probe.
				id := chi.URLParam(r, "id")
				sb, ok := ownedSandbox(w, r, mgr, ownerID)
				if !ok {
					return
				}
				status, _ := sb.statusSnapshot()
				state, upstream, hasUpstream := localPreviewState(status, sb.appStartedSnapshot(), func() (int, error) {
					return localPreviewProbe(r.Context(), sb.Port)
				})
				out := map[string]interface{}{"state": state, "sandbox": id, "port": sb.Port}
				if hasUpstream {
					out["upstream_status"] = upstream
				}
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, out)
			})
		})

		// Live resource usage; explicit route so the proxy engages.
		r.Get("/v1/sandboxes/{id}/metrics", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				// No control plane locally. 200 with an empty object, so the
				// dashboard's poll does not treat a normal launch as broken.
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, map[string]interface{}{"id": chi.URLParam(r, "id")})
			})
		})

		// Best-effort port detection on the node; explicit route so the proxy engages.
		r.Get("/v1/sandboxes/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				// No control plane locally; an empty list engages the client's
				// manual-port fallback.
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, map[string]interface{}{"ports": []interface{}{}})
			})
		})

		// The fleet shape; the dashboard and SDKs read "sandboxes".
		r.Get("/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			writeJSON(w, map[string]interface{}{"sandboxes": mgr.List(ownerID(r))})
		})

		// The catalog every launch path in the dashboard starts from. The fleet's
		// answer marks which templates some node can boot (available).
		r.Get("/v1/templates", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { serveCatalog(w, r) })
		})

		registerFilesRoutes(r, mgr, ownerID)

		registerAPIKeyRoutes(r, keyStore, ownerID)

		// Vault: provider keys stored encrypted, listed by name only,
		// injected into creates via secret_refs.
		registerVaultRoutes(r, vault, ownerID)

		// Optional: mounts nothing without a store.
		registerProjectRoutes(r, projectStore, ownerID)

		r.Get("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { writeJSON(w, usage.Stats()) })
		})

		r.Get("/v1/events", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { writeBytes(w, []byte(`{"events":[]}`)) })
		})
		r.Get("/v1/concurrency", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { writeBytes(w, []byte(`{"points":[]}`)) })
		})

		// Custom image build, relayed to the orchestrator with body and status untouched.
		r.Post("/v1/images/build", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) {
				http.Error(w, `{"error":"image build requires the cloud fleet"}`, http.StatusServiceUnavailable)
			})
		})

		r.Get("/v1/history", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { writeBytes(w, []byte(`{"history":[]}`)) })
		})
		r.Get("/v1/history/{id}", func(w http.ResponseWriter, r *http.Request) {
			fleetProxy(w, r, func(w http.ResponseWriter) { http.Error(w, `{"error":"not found"}`, http.StatusNotFound) })
		})

		// Stub so the SPA's host widget works on the local backend.
		r.Get("/v1/host", hostStub(mgr, devKey))

		// After the fleet-passthrough /metrics stub above: chi lets the last
		// registration of a pattern win, and the local ownership-checked
		// handler must be the one that wins.
		registerSandboxActionRoutes(r, mgr, ownerID, execHistory, usage)

		registerFilesystemRoutes(r, mgr, ownerID)

	}) // end auth group

	// Interactive landing page for each local sandbox.
	r.Get("/sandbox/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		mgr.mu.RLock()
		sb, ok := mgr.sandboxes[id]
		mgr.mu.RUnlock()
		if !ok || sb.UserID != ownerID(r) {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
		serveSandboxPage(w, sb, "http://"+r.Host)
	})
	r.Get("/sandbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sandbox/"+chi.URLParam(r, "id")+"/", http.StatusMovedPermanently)
	})

	// In fleet mode /preview/... goes to the orchestrator, which
	// reverse-proxies to the owning node; preview auth lives there too.
	var previewRP http.Handler
	if proxyMode {
		if previewRP = previewReverseProxy(orchURL); previewRP != nil {
			pathRP := pathPreviewRedirect(previewRP)
			r.Handle("/preview/{id}/{port}/*", pathRP)
			r.Handle("/preview/{id}/{port}", pathRP)
		}
	} else {
		// Local mode: the app is on the sandbox's assigned localhost port, and
		// the terminal is the envd web terminal every local sandbox shares.
		local := localPreview(mgr, ownerID)
		r.Handle("/preview/{id}/{port}/*", local)
		r.Handle("/preview/{id}/{port}", local)
	}

	addr, warnPublic := safeBindAddr(listenAddr, requireAuth,
		config.Bool("PORTAL_ALLOW_PUBLIC_ANON", false))
	if warnPublic {
		fmt.Fprintln(os.Stderr, "SECURITY WARNING: devserver is exposed on a public interface with anonymous access enabled (PORTAL_ALLOW_PUBLIC_ANON=1). Set PORTAL_API_KEY to require authentication.")
	}
	srv := &http.Server{
		Addr: addr,
		// <port>-<id>.<domain> hosts serve the sandbox app; everything else
		// falls through to the site router.
		Handler: portalHandler(&proxy.LegacyDomains{
			Current: portalSandboxDomain, Domains: legacyDomains,
			Store: func() *pdstore.Store { return projectStore },
		}, previewRP, r, storeSlugLookup(projectStore)),
		// No WriteTimeout: it would cap every SSE stream (log feed, streaming
		// exec). IdleTimeout and per-handler contexts bound the rest.
		IdleTimeout: 60 * time.Second,
		// Bounds slow-header clients before any auth runs.
		ReadHeaderTimeout: 10 * time.Second,
	}

	listeners, err := listenLocal(bootCtx, addr)
	if err != nil {
		// Not Fatal: returning runs the deferred stopEnvd, or envd outlives us holding its port.
		log.Error("cannot listen", zap.String("addr", addr), zap.Error(err))
		return
	}
	_, port, _ := net.SplitHostPort(addr)
	base := "http://localhost:" + port
	for _, ln := range listeners[1:] {
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatal("server error", zap.Error(err))
			}
		}()
	}
	go func() {
		log.Info("server_ready",
			zap.String("dashboard", base+"/dashboard"),
			zap.String("api", base+"/v1/sandboxes"),
			zap.String("dev_key", devKey),
		)
		fmt.Println()
		fmt.Printf("\n  PillDrops dev server (local runtime: commands run as you, unisolated)\n\n"+
			"    Dashboard  %[1]s/dashboard\n    API        %[1]s/v1/sandboxes\n    Health     %[1]s/health\n\n"+
			"    Try: curl -X POST %[1]s/v1/sandboxes -d '{\"template\":\"python-3.11\"}'\n\n"+
			"  Press Ctrl+C to stop\n\n", base)
		if devKey != "" {
			fmt.Printf("  API key: %s\n\n", devKey)
		}
		if err := srv.Serve(listeners[0]); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("shutting_down")

	mgr.mu.RLock()
	ids := slices.Collect(maps.Keys(mgr.sandboxes))
	mgr.mu.RUnlock()
	for _, id := range ids {
		mgr.Destroy(context.Background(), id)
	}
	log.Info("destroyed_all_sandboxes", zap.Int("count", len(ids)))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("shutdown_error", zap.Error(err))
	}
}

func genID() string {
	ts := time.Now().UnixMilli()
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("sb_%d_%x", ts, b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func serveSandboxPage(w http.ResponseWriter, sb *DevSandbox, baseURL string) {
	lang, icon, bin := "Python", "🐍", "python3"
	if strings.Contains(sb.Template, "node") {
		lang, icon, bin = "Node.js", "🟢", "node"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeFprintf(w, `<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Sandbox %s</title>
<style>
:root{--bg:#0a0a0a;--card:#141210;--border:#2D2925;--orange:#FF8800;--text:#A6A29E;--bright:#F5F2EE;--green:#4ADE80;--red:#F87171;--blue:#60A5FA;--mono:'SF Mono','Fira Code',monospace}
*{box-sizing:border-box;margin:0;padding:0}body{font-family:Inter,-apple-system,system-ui,sans-serif;background:var(--bg);color:var(--text);min-height:100vh}
.hdr{background:var(--card);border-bottom:1px solid var(--border);padding:14px 28px;display:flex;align-items:center;gap:14px}
.logo{color:var(--orange);font-weight:700;font-size:18px}.idb{font-family:var(--mono);font-size:13px;color:var(--bright);background:#1E1A16;padding:4px 12px;border-radius:6px;border:1px solid var(--border)}
.tpl{font-size:12px;font-weight:600;padding:3px 10px;border-radius:12px;background:rgba(255,136,0,.15);color:var(--orange)}
.tag{display:inline-block;padding:2px 8px;border-radius:4px;font-size:11px;font-weight:600;background:rgba(34,197,94,.15);color:var(--green)}
.bi{font-size:12px;color:#77746F;margin-left:auto}
.ct{max-width:1100px;margin:0 auto;padding:28px}
h3{color:var(--bright);font-size:15px;font-weight:600;margin-bottom:10px}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:20px;margin-bottom:24px}
.card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:20px}
.url{display:flex;align-items:center;gap:8px;background:#1E1A16;border:1px solid var(--border);border-radius:8px;padding:8px 14px;margin-bottom:18px}
.url code{font-family:var(--mono);font-size:13px;color:var(--bright);flex:1}.url button{background:none;border:none;color:var(--text);cursor:pointer;font-size:14px;padding:2px 6px}
.url button:hover{color:var(--orange)}
.out{background:#0D0D0D;border:1px solid var(--border);border-radius:8px;padding:14px;min-height:180px;max-height:250px;overflow-y:auto;font-family:var(--mono);font-size:13px}
.o-cmd{color:var(--orange)}.o-out{color:var(--green);white-space:pre-wrap;word-break:break-all}.o-err{color:var(--red);white-space:pre-wrap}
.ir{display:flex;gap:8px;margin-top:10px}
.ir input{flex:1;background:#1E1A16;border:1px solid var(--border);border-radius:6px;padding:8px 12px;color:var(--bright);font-family:var(--mono);font-size:13px;outline:none}
.ir input:focus{border-color:var(--orange)}
.ir button{background:var(--orange);color:#000;font-weight:600;border:none;padding:8px 20px;border-radius:6px;cursor:pointer;font-size:13px}
.pc{display:flex;gap:6px;flex-wrap:wrap;margin-top:8px}
.pc span{background:#1E1A16;border:1px solid var(--border);padding:3px 8px;border-radius:6px;font-family:var(--mono);font-size:11px;color:var(--text);cursor:pointer}
.pc span:hover{border-color:var(--orange);color:var(--orange)}
.ab{background:#0D0D0D;border-radius:8px;padding:12px 14px;margin-bottom:10px;font-family:var(--mono);font-size:12px;color:var(--text);position:relative;overflow-x:auto;white-space:pre}
.cb{position:absolute;top:6px;right:6px;background:#2D2925;border:none;color:var(--text);padding:2px 8px;border-radius:4px;cursor:pointer;font-size:11px}
.cb:hover{color:var(--bright)}
.sl{font-size:11px;font-weight:500;text-transform:uppercase;letter-spacing:.06em;color:#57544F;margin-bottom:6px}
.mp{color:var(--blue);font-weight:700}.md{color:var(--red);font-weight:700}.mg{color:var(--green);font-weight:700}
</style></head><body>
<div class="hdr"><span class="logo">PillDrops</span><span class="idb">%s</span><span class="tpl">%s %s</span><span class="tag">● Running</span><span class="bi">Boot: %dms · %s</span></div>
<div class="ct">
<div class="url"><span style="color:var(--orange)">🔗</span><code id="pageUrl">%s/sandbox/%s/</code>
<button onclick="navigator.clipboard.writeText(document.getElementById('pageUrl').textContent)" title="Copy URL">📋</button>
<button onclick="window.open('%s/v1/sandboxes/%s/exec','_blank')" title="Open API">↗</button></div>

<div class="grid"><div class="card">
<h3>%s %s — Interactive Terminal</h3><div class="sl">Run commands in this sandbox</div>
<div class="out" id="O"></div>
<div class="ir"><input type="text" id="I" placeholder="Type a command..." autofocus /><button onclick="run()">Run ▶</button></div>
<div class="pc">
<span onclick="qr('echo Hello World!')">echo Hello</span>
<span onclick="qr('date')">date</span>
<span onclick="qr('whoami')">whoami</span>
<span onclick="qr('uname -a')">uname</span>
<span onclick="qr('%s --version')">%s version</span>
<span onclick="qr('ls -la /tmp')">ls /tmp</span>
<span onclick="qr('cat /etc/os-release')">os-release</span>
</div></div>

<div class="card">
<h3>🔌 API Reference</h3><div class="sl">curl commands for this sandbox</div>
<div class="ab"><button class="cb" onclick="cc(this)">Copy</button><span class="mp">POST</span> %s/v1/sandboxes/%s/exec
  -H 'Content-Type: application/json'
  -d '{"cmd":"echo hello","cwd":"/tmp"}'</div>
<div class="ab"><button class="cb" onclick="cc(this)">Copy</button><span class="mp">POST</span> %s/v1/sandboxes
  -d '{"template":"%s"}'</div>
<div class="ab"><button class="cb" onclick="cc(this)">Copy</button><span class="md">DELETE</span> %s/v1/sandboxes/%s</div>
<div class="ab"><button class="cb" onclick="cc(this)">Copy</button><span class="mg">GET</span> %s/health</div>
<div class="ab"><button class="cb" onclick="cc(this)">Copy</button><span class="mg">GET</span> %s/dashboard</div>
</div></div></div>

<script>
var B='%s',ID='%s',O=document.getElementById('O'),I=document.getElementById('I');
I.addEventListener('keydown',function(e){if(e.key==='Enter'&&I.value.trim())run()});
function qr(c){I.value=c;run()}
async function run(){var c=I.value.trim();if(!c)return;I.value='';O.innerHTML+='<div class="o-cmd">$ '+esc(c)+'</div>';O.scrollTop=O.scrollHeight;
try{var r=await fetch(B+'/v1/sandboxes/'+ID+'/exec',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({cmd:c,cwd:'/tmp'})});
var d=await r.json();if(d.stdout)O.innerHTML+='<div class="o-out">'+esc(d.stdout)+'</div>';if(d.stderr)O.innerHTML+='<div class="o-err">'+esc(d.stderr)+'</div>';
if(d.exit_code&&d.exit_code!==0)O.innerHTML+='<div style="color:#77746F">[exit: '+d.exit_code+']</div>';if(d.error)O.innerHTML+='<div class="o-err">'+esc(d.error)+'</div>';
}catch(e){O.innerHTML+='<div class="o-err">Error: '+esc(e.message)+'</div>';}O.scrollTop=O.scrollHeight;}
function esc(s){var d=document.createElement('div');d.textContent=s;return d.innerHTML}
function cc(b){var t=b.parentElement.textContent.replace('Copy','').trim();navigator.clipboard.writeText('curl -X '+t).then(function(){b.textContent='Copied!';setTimeout(function(){b.textContent='Copy'},1500)})}
</script></body></html>`,
		sb.ID,             // title
		sb.ID, icon, lang, // header
		sb.BootMs, sb.BootType, // boot info
		baseURL, sb.ID, // url bar
		baseURL, sb.ID, // url bar buttons
		icon, lang, // try it title
		bin, lang, // quick commands
		baseURL, sb.ID, // exec API block
		baseURL, sb.Template, // create API block
		baseURL, sb.ID, // delete API block
		baseURL,        // health
		baseURL,        // dashboard
		baseURL, sb.ID, // JS vars
	)
}

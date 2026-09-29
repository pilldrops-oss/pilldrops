package orchestrator

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
)

// The per-sandbox agent origin, agent-<id>.<PORTAL_SANDBOX_DOMAIN>, is a
// separate origin so the browser keeps share-link cookies and agent output
// isolated from the dashboard. It does not authenticate: the node validates
// the token, so this tier rate limits instead.

// agentOriginRPS bounds unauthenticated requests per client IP on this origin.
const (
	agentOriginRPS   = 5
	agentOriginBurst = 30
)

// ipLimiter is a token bucket per client IP, swept opportunistically on write.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     float64
	burst   float64
	now     func() time.Time // injectable so the tests do not sleep
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(rps, burst float64) *ipLimiter {
	return &ipLimiter{buckets: map[string]*bucket{}, rps: rps, burst: burst, now: time.Now}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[ip]
	if !ok {
		// A bucket idle long enough to have refilled is identical to a fresh one.
		if len(l.buckets) > 1024 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > time.Duration(l.burst/l.rps)*time.Second {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rps)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// agentOrigin wraps the router so agent-<id>.<domain> serves one sandbox's
// agent surface; every other host passes through to next.
func (s *Server) agentOrigin(domain string, next http.Handler) http.Handler {
	if domain == "" {
		return next
	}
	lim := newIPLimiter(agentOriginRPS, agentOriginBurst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := agentHostID(r.Host, domain)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// The org's source-network restriction covers this origin too. Before
		// the rate limiter and before any hint the id is real: the same 404 an
		// unknown sandbox gets, so a restricted org cannot be enumerated here
		// any more than it can through the preview path.
		if org, ok := s.previewOrgOf(r.Context(), id); ok && !s.peerAllowed(r, org) {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		if !lim.allow(httpx.ClientIP(r)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		agentOriginHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveAgentViewer(w, r, id)
			return
		}

		target, ok := proxy.AgentRequestPath(id, r.URL.Path)
		if !ok {
			// Never fall through: this origin must not reach the org-key API or dashboard.
			http.Error(w, `{"error":"not an agent endpoint on this origin"}`, http.StatusNotFound)
			return
		}
		if target == "/agent/claim" {
			// Only the sandbox-scoped form can be routed to a node.
			target = "/v1/sandboxes/" + id + "/agent/claim"
		}

		r2 := r.Clone(r.Context())
		r2.URL.Path = target
		r2.URL.RawPath = ""
		liftAgentToken(r2)
		// Overwritten, not passed through: the node uses these to scope the claim
		// cookie, so a client must not choose them. Proto still trusts the edge's
		// X-Forwarded-Proto; forging it only weakens the forger's own cookie.
		r2.Header.Set("X-Forwarded-Host", proxy.NormalizeHost(r.Host))
		r2.Header.Set("X-Forwarded-Proto", requestProto(r))

		// No X-Org-Override: the node derives the org from the token.
		s.serveAgentOriginProxy(w, r2, id)
	})
}

// liftAgentToken moves an AAT from the bearer header or claim cookie into
// X-Agent-Token, since the next hop overwrites Authorization with the platform
// key. Any inbound X-Agent-Token is dropped first so the header always means
// "this tier parsed it", never "the client asserted it".
func liftAgentToken(r *http.Request) {
	r.Header.Del(agentTokenHeader)

	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == r.Header.Get("Authorization") {
		tok = ""
	}
	if !strings.HasPrefix(tok, aat.Prefix) {
		tok = ""
	}
	if tok == "" {
		if c, err := r.Cookie(agentCookieName); err == nil && strings.HasPrefix(c.Value, aat.Prefix) {
			tok = c.Value
		}
	}
	// Neither the caller's Authorization nor its cookies may reach a machine running customer code.
	r.Header.Del("Authorization")
	r.Header.Del("Cookie")
	if tok != "" {
		r.Header.Set(agentTokenHeader, tok)
	}
}

// agentCookieName mirrors internal/httpapi's agentTokenCookie; the tier guard forbids importing it.
const agentCookieName = "sandboxd_agent"

// agentTokenHeader mirrors internal/httpapi.AgentTokenHeader; the tier guard forbids importing it.
const agentTokenHeader = "X-Agent-Token"

// agentOriginHeaders applies the isolation this origin exists to provide.
func agentOriginHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Same-origin only, so an injection carried in agent output cannot call home.
	h.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline'", // the viewer is one inline script
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"frame-ancestors 'none'", // never inside the dashboard's document
		"form-action 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	}, "; "))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	// A share link must never leak through the referrer.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	// CORS: same-origin only, echoed rather than "*" because "*" is incompatible with credentials.
	if o := r.Header.Get("Origin"); o != "" && sameOrigin(o, r) {
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Last-Event-ID, Idempotency-Key")
		h.Set("Vary", "Origin")
	}
}

// sameOrigin reports whether an Origin header names this same request's host.
func sameOrigin(origin string, r *http.Request) bool {
	_, rest, found := strings.Cut(origin, "://")
	if !found {
		return false
	}
	return proxy.NormalizeHost(rest) == proxy.NormalizeHost(r.Host)
}

// agentHostID extracts the sandbox id from agent-<id>.<domain> using the node's parser.
func agentHostID(host, domain string) (string, bool) {
	return proxy.ParseAgentHost(proxy.NormalizeHost(host), strings.ToLower(domain))
}

func requestProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	return "http"
}

// serveAgentOriginProxy forwards an agent-origin request to the node that owns
// the sandbox. No org is asserted; the node derives it from the token.
func (s *Server) serveAgentOriginProxy(w http.ResponseWriter, r *http.Request, id string) {
	node, ok := s.reg.NodeFor(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	// Keep the query: /agent/claim carries the share code in ?code=.
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	resp, err := s.forwardStreamRelaying(r.Context(), node, r.Method, path, r.Body,
		r.Header.Get("Content-Type"), r.Header, agentOriginRelayHeaders, "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed below
	relay(w, resp)
}

// agentRelayHeaders are the inbound headers relayed to the node. An allowlist,
// not a blanket copy: Authorization holds a customer credential. Dropping
// Last-Event-ID would make every stream reconnect silently restart at the live edge.
var agentRelayHeaders = []string{"Last-Event-ID", "Idempotency-Key", "Accept"}

// agentOriginRelayHeaders is agentRelayHeaders plus the headers only the
// per-sandbox agent origin may relay; X-Agent-Token must not be relayable on the shared host.
var agentOriginRelayHeaders = append(append([]string{}, agentRelayHeaders...),
	agentTokenHeader, "X-Forwarded-Host", "X-Forwarded-Proto")

package orchestrator

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Wildcard-host ingress serves <port>-<id>.<domain> (the sandbox app).
// A separate origin per sandbox is the browser's own boundary; the path form
// would put sandbox code on the dashboard's origin.

// hostIngress wraps the router with wildcard-host routing; with no domain
// configured it returns next unchanged. Customer hostnames are last: the
// platform's own forms win, and a host nobody attached still falls through to
// the dashboard.
func (s *Server) hostIngress(next http.Handler) http.Handler {
	domain := strings.TrimSpace(s.sandboxDomain)
	if domain == "" {
		return next
	}
	return s.legacy.Wrap(s.appPreviewOrigin(domain, s.customDomainOrigin(s.legacy.SandboxDomainOnly(domain, s.portalOrigin, next))))
}

// setLegacyDomains installs the redirect from domains to sandboxDomain.
func (s *Server) setLegacyDomains(domains []string) {
	s.legacy = &proxy.LegacyDomains{
		Current: strings.ToLower(strings.TrimSpace(s.sandboxDomain)),
		Domains: domains,
		Store:   s.st.Load,
		Now:     func() time.Time { return s.now() },
	}
}

// appPreviewOrigin rewrites <port>-<id>.<domain>, or <slug>.<domain> at the
// sandbox's public port, into the path form handlePreview already serves. The
// parsers are internal/proxy's, shared with the node. A slug no live sandbox
// holds falls through, so www. and friends keep reaching the site.
func (s *Server) appPreviewOrigin(domain string, next http.Handler) http.Handler {
	suffix := "." + strings.ToLower(domain)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := proxy.NormalizeHost(r.Host)
		if !strings.HasSuffix(host, suffix) {
			next.ServeHTTP(w, r)
			return
		}
		id, port, ok := proxy.ParseSandboxHost(host, domain)
		if !ok {
			id, port, ok = s.slugTarget(r, host, domain)
		}
		if !ok {
			// Not a known sandbox host; the next handler decides (custom domain or dashboard).
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, previewRewrite(proxy.WithPreviewHost(r.Context()), r, id, port))
	})
}

// previewRewrite restates a host-form request in the /preview/<id>/<port>/...
// path form handlePreview already serves.
func previewRewrite(ctx context.Context, r *http.Request, id string, port int) *http.Request {
	rest := r.URL.Path
	if rest == "" {
		rest = "/"
	}
	r2 := r.Clone(ctx)
	r2.URL.Path = "/preview/" + id + "/" + strconv.Itoa(port) + rest
	r2.URL.RawPath = ""
	return r2
}

// slugTarget resolves <slug>.<domain> against the store to the holder's id
// and public port; ok=false without a store or for a slug nobody holds.
func (s *Server) slugTarget(r *http.Request, host, domain string) (id string, port int, ok bool) {
	slug, isSlug := proxy.ParseSlugHost(host, domain)
	st := s.st.Load()
	if !isSlug || st == nil {
		return "", 0, false
	}
	rec, found, err := st.SandboxBySlug(r.Context(), slug)
	if err != nil || !found {
		return "", 0, false
	}
	port = rec.PublicPort
	if port == 0 {
		port = 8000
	}
	return rec.ID, port, true
}

// bySlug rewrites a /v1/sandboxes/{id} segment that is not a placed id but is
// one of the caller's org's slugs. Inside auth (the org is known) and before
// any forward, so a node only ever sees ids; a placed id always wins; another
// org's slug resolves to nothing and 404s like an unknown id.
func (s *Server) bySlug(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		seg := r.PathValue("id")
		if _, placed := s.reg.NodeFor(seg); !placed && sandbox.ValidSlug(seg) == nil {
			if st := s.st.Load(); st != nil {
				if rec, ok, err := st.SandboxBySlug(r.Context(), seg); err == nil && ok && rec.OrgID == fastOrg(r) {
					r.SetPathValue("id", rec.ID)
				}
			}
		}
		next(w, r)
	}
}

// --- custom domains -------------------------------------------------------

// customDomainTTL bounds how stale a routing decision may be. The store is the
// truth across replicas; this only keeps a hot host off the database.
const customDomainTTL = 30 * time.Second

// hostRoute is one cached routing decision. owned means the host is one of
// ours (an exact row, or a label under an active wildcard), so a request that
// names no live sandbox is answered 404 instead of falling through to the
// dashboard: it is the customer's own domain, not ours to serve a site on.
type hostRoute struct {
	id    string
	port  int
	owned bool
	// row is the custom_domains host the route came from; legacy says its DNS
	// still names a legacy sandbox domain, so serving it is recorded.
	row    string
	legacy bool
	exp    time.Time
}

// routeCache is the per-process custom-domain cache, negative entries included.
// Zero value ready; the map is created on first write.
type routeCache struct {
	mu sync.Mutex
	m  map[string]hostRoute
}

func (c *routeCache) get(host string, now time.Time) (hostRoute, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[host]
	if !ok || now.After(r.exp) {
		return hostRoute{}, false
	}
	return r, true
}

func (c *routeCache) put(host string, r hostRoute) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]hostRoute{}
	}
	// A bound, not a policy: entries expire in 30 s, and a flood of distinct
	// Host headers must not grow the map without limit.
	if len(c.m) > 10000 {
		c.m = map[string]hostRoute{}
	}
	c.m[host] = r
}

func (c *routeCache) forget(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, host)
}

// customDomainOrigin is the last host stage: a verified customer host is
// rewritten into the path form handlePreview already serves, exactly as the
// platform wildcard is. Anything else falls through, so the dashboard keeps
// answering on its own hostname.
func (s *Server) customDomainOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := s.customRoute(r, proxy.NormalizeHost(r.Host))
		if !route.owned {
			next.ServeHTTP(w, r)
			return
		}
		if route.id == "" {
			// Attached or under a customer wildcard, but naming no sandbox of
			// theirs: the same 404 handlePreview gives an unknown id, never the
			// dashboard on someone else's domain.
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		if route.legacy {
			s.noteLegacyTarget(r.Context(), route.row)
		}
		next.ServeHTTP(w, previewRewrite(withCustomHost(r.Context()), r, route.id, route.port))
	})
}

// customRoute resolves a hostname to a sandbox and port through the domain
// table, cached for customDomainTTL. A zero route means "not ours".
func (s *Server) customRoute(r *http.Request, host string) hostRoute {
	if host == "" {
		return hostRoute{}
	}
	if cached, ok := s.domains.get(host, s.now()); ok {
		return cached
	}
	route := s.lookupCustomRoute(r, host)
	route.exp = s.now().Add(customDomainTTL)
	s.domains.put(host, route)
	return route
}

// lookupCustomRoute reads the store: an active exact row first, then the active
// wildcard rows by suffix. A wildcard label is resolved the same way the
// platform domain resolves one, and must name a sandbox of the row's own org.
func (s *Server) lookupCustomRoute(r *http.Request, host string) hostRoute {
	st := s.st.Load()
	if st == nil {
		return hostRoute{}
	}
	ctx := r.Context()
	if d, ok, err := st.GetDomain(ctx, host); err == nil && ok && !d.Wildcard {
		if d.Status != "active" {
			// Attached but unverified: not ours to serve yet.
			return hostRoute{owned: true}
		}
		return hostRoute{id: d.SandboxID, port: d.Port, owned: true, row: d.Host, legacy: d.LegacyTargetSeenAt != 0}
	}
	wilds, err := st.ActiveWildcardDomains(ctx)
	if err != nil {
		return hostRoute{}
	}
	for _, w := range wilds {
		base := strings.TrimPrefix(w.Host, "*.")
		if host != base && !strings.HasSuffix(host, "."+base) {
			continue
		}
		// The bare org domain names no sandbox, so it parses to nothing and 404s.
		id, port, ok := proxy.ParseSandboxHost(host, base)
		if !ok {
			id, port, ok = s.slugTarget(r, host, base)
		}
		// The public check the exact path takes at attach time, taken here
		// instead: a wildcard row names no sandbox when it is stored, so
		// nothing ever asked whether the sandbox it resolves to opted into
		// being shared. Without this a private sandbox is addressable under a
		// CUSTOMER's hostname and answers with OUR refusal page on it. 404 is
		// the same answer this path already gives a label naming no sandbox of
		// theirs — a token link under someone else's wildcard is not a thing
		// the wildcard was opted into.
		if !ok || !s.sandboxInOrg(ctx, id, w.OrgID) || !s.isPublicPreview(ctx, id) {
			return hostRoute{owned: true}
		}
		return hostRoute{id: id, port: port, owned: true, row: w.Host, legacy: w.LegacyTargetSeenAt != 0}
	}
	return hostRoute{}
}

// noteLegacyTarget records that a row still on a legacy CNAME served traffic,
// which is what keeps the verifier accepting that CNAME.
func (s *Server) noteLegacyTarget(ctx context.Context, row string) {
	st, now := s.st.Load(), s.now()
	if st == nil || !s.legacyStamps.due(row, now) {
		return
	}
	if err := st.TouchDomainLegacySeen(ctx, row, now.Unix()); err != nil {
		s.log().WarnContext(ctx, "could not record traffic on a legacy custom domain",
			"component", "domains", "host", row, "err", err)
	}
}

// stampThrottle lets each key through at most once per
// store.LegacyWriteEvery on this replica. Zero value ready.
type stampThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *stampThrottle) due(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at, ok := t.last[key]; ok && now.Sub(at) < store.LegacyWriteEvery {
		return false
	}
	// The same bound as routeCache: keys are custom-domain rows, but a map
	// fed by traffic must never grow without limit.
	if t.last == nil || len(t.last) > 10000 {
		t.last = map[string]time.Time{}
	}
	t.last[key] = now
	return true
}

// sandboxInOrg reports whether the sandbox is live and owned by org. A wildcard
// host must never serve another org's sandbox just because the label parses.
func (s *Server) sandboxInOrg(ctx context.Context, id, org string) bool {
	if id == "" {
		return false
	}
	owner, ok := s.previewOrgOf(ctx, id)
	return ok && owner == org
}

// resolveCustomHost is the TLS ask endpoint's view of the same tables: non-nil
// only for a host that would actually route right now.
func (s *Server) resolveCustomHost(r *http.Request, host string) *hostRoute {
	route := s.customRoute(r, host)
	if route.id == "" {
		return nil
	}
	return &route
}

// customHostKey marks a request that arrived on a customer hostname, so the
// preview response can drop the dashboard from its frame-ancestors list.
type customHostKey struct{}

func withCustomHost(ctx context.Context) context.Context {
	return context.WithValue(ctx, customHostKey{}, true)
}

// onCustomHost reports whether this request came in through customDomainOrigin.
func onCustomHost(r *http.Request) bool {
	v, _ := r.Context().Value(customHostKey{}).(bool)
	return v
}

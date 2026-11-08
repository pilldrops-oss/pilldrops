package proxy

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// ParseLegacyDomains reads PORTAL_LEGACY_SANDBOX_DOMAINS: comma-separated
// domains previews moved off, whose preview hosts now only redirect to
// current. The dashboard stays behind on a legacy domain, so PORTAL_ORIGIN
// must name it, on a host the redirect leaves alone: previews refuse to be
// framed by anything else.
func ParseLegacyDomains(raw, current, portalOrigin string) ([]string, error) {
	current = strings.ToLower(strings.TrimSpace(current))
	var out []string
	for _, d := range strings.Split(raw, ",") {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" {
			continue
		}
		if current == "" {
			return nil, fmt.Errorf("PORTAL_LEGACY_SANDBOX_DOMAINS needs PORTAL_SANDBOX_DOMAIN: there is no domain to redirect %s to", d)
		}
		if d == current || strings.HasSuffix(d, "."+current) || strings.HasSuffix(current, "."+d) {
			return nil, fmt.Errorf("legacy sandbox domain %s overlaps PORTAL_SANDBOX_DOMAIN %s, so a host could redirect to itself", d, current)
		}
		out = append(out, d)
	}
	if len(out) > 0 && strings.TrimSpace(portalOrigin) == "" {
		return nil, fmt.Errorf("PORTAL_LEGACY_SANDBOX_DOMAINS moves previews off the dashboard's domain, so PORTAL_ORIGIN " +
			"must name the dashboard (such as https://example.com): without it the dashboard cannot embed a preview")
	}
	if err := LegacyHostError(out, "PORTAL_ORIGIN", portalOrigin); err != nil {
		return nil, err
	}
	return out, nil
}

// LegacyHostError refuses a platform URL, read from setting, whose host the
// legacy redirect would take for a preview and send to the preview domain.
func LegacyHostError(domains []string, setting, rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if d, label, ok := (&LegacyDomains{Domains: domains}).previewLabel(host); ok {
		return fmt.Errorf("%s host %s is shaped like a preview under legacy sandbox domain %s, so it would be "+
			"redirected to the preview domain: move it to %s, www.%s or app.%s, or reserve the label %q for the platform",
			setting, host, d, d, d, d, label)
	}
	return nil
}

// LegacyDomains redirects preview-shaped hosts under a legacy domain to the
// same label under Current, and never serves them. Once a domain has gone
// store.LegacyUseWindow without a redirect, its hosts answer a "moved" page
// instead. A nil *LegacyDomains, or one with no Domains, changes nothing.
type LegacyDomains struct {
	Current string
	Domains []string
	// Store is read per request. Nil, or a nil store, redirects without
	// recording use or ever expiring.
	Store func() *store.Store
	Now   func() time.Time

	mu      sync.Mutex
	checked map[string]legacyCheck
}

// legacyCheck is this replica's last read of one domain's use row.
type legacyCheck struct {
	at      time.Time
	expired bool
}

// Wrap puts the redirect in front of next.
func (l *LegacyDomains) Wrap(next http.Handler) http.Handler {
	if l == nil || len(l.Domains) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain, label, ok := l.previewLabel(NormalizeHost(r.Host))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// A use is counted only when it reaches a server, so no browser may cache the answer.
		w.Header().Set("Cache-Control", "no-store")
		if l.expired(r.Context(), domain) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusGone)
			fmt.Fprintf(w, legacyMovedPage, html.EscapeString(l.Current))
			return
		}
		// 308, not 301: it keeps the method and body, so a webhook POST arrives intact.
		http.Redirect(w, r, "https://"+label+"."+l.Current+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// SandboxHost parses <port>-<id> under any legacy domain.
func (l *LegacyDomains) SandboxHost(host string) (id string, port int, ok bool) {
	if l == nil {
		return "", 0, false
	}
	for _, d := range l.Domains {
		if id, port, ok = ParseSandboxHost(host, d); ok {
			return id, port, true
		}
	}
	return "", 0, false
}

// previewLabel finds the legacy domain host is one preview-shaped label under.
// Platform labels (www, api, login, docs...) are not preview-shaped, so the
// site, dashboard and sign-in on a legacy domain keep answering.
func (l *LegacyDomains) previewLabel(host string) (domain, label string, ok bool) {
	for _, d := range l.Domains {
		if _, _, ok := ParseSandboxHost(host, d); ok {
			return d, strings.TrimSuffix(host, "."+d), true
		}
		if slug, ok := ParseSlugHost(host, d); ok && !sandbox.ReservedForNewSlugs(slug) {
			return d, slug, true
		}
	}
	return "", "", false
}

func (l *LegacyDomains) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// expired reports whether domain has gone store.LegacyUseWindow without a
// redirect, and records this use when it has not. A visit after expiry does
// not revive the domain. The store is read, and written, at most once per
// store.LegacyWriteEvery per domain on this replica.
func (l *LegacyDomains) expired(ctx context.Context, domain string) bool {
	now := l.now()
	l.mu.Lock()
	c, seen := l.checked[domain]
	l.mu.Unlock()
	if seen && now.Sub(c.at) < store.LegacyWriteEvery {
		return c.expired
	}
	var st *store.Store
	if l.Store != nil {
		st = l.Store()
	}
	if st == nil {
		return false
	}
	expired := false
	last, used, err := st.LegacyDomainLastUsed(ctx, domain)
	switch {
	case err != nil:
		// Fail open: a redirect carries no sandbox content, and a wrong "moved" page breaks a working link.
		slog.WarnContext(ctx, "legacy domain use unreadable, redirecting", "component", "preview", "domain", domain, "err", err)
	case used && now.Sub(time.Unix(last, 0)) > store.LegacyUseWindow:
		expired = true
	default:
		if err := st.TouchLegacyDomain(ctx, domain, now.Unix()); err != nil {
			slog.WarnContext(ctx, "could not record a legacy domain use", "component", "preview", "domain", domain, "err", err)
		}
	}
	l.mu.Lock()
	if l.checked == nil {
		l.checked = map[string]legacyCheck{}
	}
	l.checked[domain] = legacyCheck{at: now, expired: expired}
	l.mu.Unlock()
	return expired
}

// legacyMovedPage is what an expired legacy preview host answers. %s is the
// current domain, escaped.
const legacyMovedPage = `<!doctype html><meta charset="utf-8"><meta name="robots" content="noindex"><title>This link has moved</title>
<style>html,body{margin:0;height:100%%;display:grid;place-items:center;background:#0e0e10;color:#e8eaed;font-family:system-ui,Segoe UI,Roboto,sans-serif}
.c{max-width:440px;text-align:center;padding:32px}h1{font-size:19px;margin:0 0 10px}p{color:#9aa0a6;font-size:14px;line-height:1.5;margin:0}</style>
<div class="c"><h1>This sandbox link has moved</h1>
<p>Sandbox links now live on <b>%s</b>. Ask whoever shared this one for a new link, or open the sandbox from the dashboard.</p></div>`

// DashboardOffDomain reports whether the dashboard at origin lives outside
// domain, which is when the preview domain must serve previews and nothing else.
func DashboardOffDomain(domain, origin string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	u, err := url.Parse(origin)
	if d == "" || err != nil || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h != d && !strings.HasSuffix(h, "."+d)
}

// WarnDashboardOnPreviewDomain logs when previews move off legacy domains but
// the dashboard at origin stays on the preview domain, beside sandbox code.
func WarnDashboardOnPreviewDomain(legacy []string, domain, origin string) {
	if len(legacy) > 0 && !DashboardOffDomain(domain, origin) {
		slog.Warn("previews move off the legacy domains, but PORTAL_ORIGIN is on PORTAL_SANDBOX_DOMAIN, "+
			"so the dashboard stays on the preview domain", "component", "preview",
			"portal_origin", origin, "sandbox_domain", domain)
	}
}

// previewHostKey marks a request a host stage already resolved to a sandbox.
type previewHostKey struct{}

// WithPreviewHost marks ctx as carrying a request a host stage resolved to a sandbox.
func WithPreviewHost(ctx context.Context) context.Context {
	return context.WithValue(ctx, previewHostKey{}, true)
}

// SandboxDomainOnly keeps the dashboard and its sign-in off the preview domain
// when previews moved off a legacy domain and the dashboard lives at origin,
// elsewhere. The domain itself and www redirect there; any other host under it
// that no stage resolved to a sandbox is a 404. Sandbox code may set cookies
// for the whole preview domain, so nothing of the dashboard's may be served
// under it. Without legacy domains it changes nothing.
func (l *LegacyDomains) SandboxDomainOnly(domain, origin string, next http.Handler) http.Handler {
	if l == nil || len(l.Domains) == 0 || !DashboardOffDomain(domain, origin) {
		return next
	}
	d := strings.ToLower(strings.TrimSpace(domain))
	origin = strings.TrimRight(origin, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := NormalizeHost(r.Host)
		resolved, _ := r.Context().Value(previewHostKey{}).(bool)
		switch {
		case resolved || (host != d && !strings.HasSuffix(host, "."+d)):
			next.ServeHTTP(w, r)
		case host == d || host == "www."+d:
			http.Redirect(w, r, origin+r.URL.RequestURI(), http.StatusFound)
		default:
			http.Error(w, "sandbox not found", http.StatusNotFound)
		}
	})
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/proxy"
)

// The portal rewrites <port>-<id>.<domain> and, when the store knows the slug,
// <slug>.<domain> into the /preview path; everything else reaches the site.
func TestHostPreviewSlugAndIDForms(t *testing.T) {
	var rpPath, sitePath string
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rpPath = r.URL.Path })
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sitePath = r.URL.Path })
	lookup := func(_ context.Context, slug string) (string, int, bool) {
		if slug == "myapp" {
			return "sb1", 3000, true
		}
		return "", 0, false
	}
	h := hostPreview("pilldrops.com", rp, site, lookup)

	get := func(host, path string) {
		rpPath, sitePath = "", ""
		req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	get("myapp.pilldrops.com", "/a?token=t")
	if rpPath != "/preview/sb1/3000/a" {
		t.Fatalf("slug host → %q (site %q), want /preview/sb1/3000/a", rpPath, sitePath)
	}
	get("8000-sb1.pilldrops.com", "/b")
	if rpPath != "/preview/sb1/8000/b" {
		t.Fatalf("id host → %q, want /preview/sb1/8000/b", rpPath)
	}
	for _, host := range []string{"nope.pilldrops.com", "www.pilldrops.com", "pilldrops.com"} {
		get(host, "/site")
		if sitePath != "/site" || rpPath != "" {
			t.Errorf("%s: site %q rp %q, want the site untouched", host, sitePath, rpPath)
		}
	}
	// No store: slug hosts fall through rather than guess.
	h = hostPreview("pilldrops.com", rp, site, nil)
	get("myapp.pilldrops.com", "/c")
	if sitePath != "/c" || rpPath != "" {
		t.Fatalf("no lookup: site %q rp %q", sitePath, rpPath)
	}
}

// An old preview link reaching the portal is redirected before any routing:
// it reaches neither the orchestrator nor the site.
func TestPortalRedirectsLegacyPreviewHosts(t *testing.T) {
	withDomain(t, "pilldrops.app")
	var rpHits, siteHits int
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rpHits++ })
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { siteHits++ })
	h := portalHandler(&proxy.LegacyDomains{Current: "pilldrops.app", Domains: []string{"pilldrops.com"}}, rp, nil, site, nil)

	req := httptest.NewRequest(http.MethodGet, "http://x/a?token=t", nil)
	req.Host = "8000-sb1.pilldrops.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://8000-sb1.pilldrops.app/a?token=t" {
		t.Fatalf("legacy host = %d %q, want 308 to pilldrops.app", w.Code, w.Header().Get("Location"))
	}
	if rpHits+siteHits != 0 {
		t.Fatalf("a legacy preview host was routed: rp %d site %d", rpHits, siteHits)
	}

	// The dashboard on the legacy domain still reaches the site.
	req = httptest.NewRequest(http.MethodGet, "http://x/dashboard", nil)
	req.Host = "pilldrops.com"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if siteHits != 1 {
		t.Fatalf("site hits = %d, want 1", siteHits)
	}
}

// With the dashboard elsewhere the portal serves nothing but previews under the
// sandbox domain: the apex goes to the dashboard and an unknown label is a 404.
func TestPortalKeepsTheDashboardOffThePreviewDomain(t *testing.T) {
	withDomain(t, "pilldrops.app")
	withOrigin(t, "https://pilldrops.com")
	var rpHits, siteHits int
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rpHits++ })
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { siteHits++ })
	h := portalHandler(nil, rp, nil, site, nil)

	do := func(host, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// With no legacy domain configured nothing moved, so the site still answers.
	do("pilldrops.app", "/dashboard")
	do("nope.pilldrops.app", "/")
	if siteHits != 2 {
		t.Fatalf("site hits = %d with no legacy domain, want the site served as before", siteHits)
	}
	siteHits = 0

	h = portalHandler(&proxy.LegacyDomains{Current: "pilldrops.app", Domains: []string{"pilldrops.com"}}, rp, nil, site, nil)
	if w := do("pilldrops.app", "/dashboard"); w.Code != http.StatusFound || w.Header().Get("Location") != "https://pilldrops.com/dashboard" {
		t.Errorf("apex = %d %q, want 302 to the dashboard", w.Code, w.Header().Get("Location"))
	}
	if w := do("nope.pilldrops.app", "/"); w.Code != http.StatusNotFound {
		t.Errorf("unknown label = %d, want 404", w.Code)
	}
	if siteHits != 0 {
		t.Fatalf("the site was served under the preview domain %d times", siteHits)
	}
	do("8000-sb1.pilldrops.app", "/")
	do("pilldrops.com", "/dashboard")
	if rpHits != 1 || siteHits != 1 {
		t.Fatalf("rp %d site %d, want the preview host to the orchestrator and the dashboard host to the site", rpHits, siteHits)
	}
}

// A browser opening the path form is sent to the host form; the CLI's
// terminal WebSocket, which sends no fetch metadata, is served as before.
func TestPortalSendsPathFormNavigationsToTheHostForm(t *testing.T) {
	withDomain(t, "pilldrops.app")
	hits := 0
	h := pathPreviewRedirect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))

	req := httptest.NewRequest(http.MethodGet, "http://x/preview/sb1/8000/a?token=t", nil)
	req.Host = "pilldrops.com"
	req.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://8000-sb1.pilldrops.app/a?token=t" {
		t.Fatalf("navigation = %d %q, want 302 to the host form", w.Code, w.Header().Get("Location"))
	}

	req = httptest.NewRequest(http.MethodGet, "http://x/preview/sb1/7681/ws", nil)
	req.Host = "pilldrops.com"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if hits != 1 {
		t.Fatalf("hits = %d, want the WebSocket served in the path form", hits)
	}
}

// The portal names the host the visitor used, overwriting any the client sent,
// so the orchestrator does not bounce a rewritten host-form request.
func TestPortalPreviewProxyNamesTheVisitorsHost(t *testing.T) {
	var got, gotOwn string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotOwn = r.Header.Get("X-Forwarded-Host"), r.Header.Get(proxy.PreviewHostHeader)
	}))
	defer up.Close()
	rp := previewReverseProxy(up.URL)
	req := httptest.NewRequest(http.MethodGet, "http://x/preview/sb1/8000/", nil)
	req.Host = "8000-sb1.pilldrops.app"
	req.Header.Set("X-Forwarded-Host", "forged.example")
	req.Header.Set(proxy.PreviewHostHeader, "forged.example")
	rp.ServeHTTP(httptest.NewRecorder(), req)
	if got != "8000-sb1.pilldrops.app" || gotOwn != "8000-sb1.pilldrops.app" {
		t.Fatalf("X-Forwarded-Host = %q, %s = %q, want the visitor's host in both", got, proxy.PreviewHostHeader, gotOwn)
	}
}

func TestPortalForwardsShareHostsToTheOrchestrator(t *testing.T) {
	withDomain(t, "pilldrops.app")
	var got *http.Request
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))
	defer orch.Close()
	var rpHits, siteHits int
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rpHits++ })
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { siteHits++ })
	h := portalHandler(nil, rp, shareHostProxy(orch.URL), site, nil)

	for _, host := range []string{"share.pilldrops.app", "share-gentle-lark-4axhhjs6.pilldrops.app"} {
		got = nil
		req := httptest.NewRequest(http.MethodGet, "http://x/tunnel?share=sh_1", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got == nil || got.Host != host || got.URL.RequestURI() != "/tunnel?share=sh_1" || rpHits+siteHits != 0 {
			t.Fatalf("share host %s: orchestrator reached %v, rp %d site %d", host, got != nil, rpHits, siteHits)
		}
	}

	// A preview host is unaffected.
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Host = "8000-sb1.pilldrops.app"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if rpHits != 1 {
		t.Fatalf("preview host hits = %d, want 1", rpHits)
	}
}

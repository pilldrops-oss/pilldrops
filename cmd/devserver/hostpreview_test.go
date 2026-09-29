package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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

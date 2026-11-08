package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// legacyServer is domainServer moved to pilldrops.app, with pilldrops.com as
// the legacy domain the dashboard stayed on.
func legacyServer(t *testing.T) *Server {
	t.Helper()
	s, _ := domainServer(t)
	s.sandboxDomain = "pilldrops.app"
	s.portalOrigin = "https://pilldrops.com"
	s.setLegacyDomains([]string{"pilldrops.com"})
	return s
}

// A bookmarked link to a private preview on the old domain: the redirect
// carries its token to the new host, which grants access there; a bookmark
// with no token lands on the private page, not in a redirect loop.
func TestLegacyRedirectCarriesAPrivatePreviewsTokenToTheNewHost(t *testing.T) {
	s := legacyServer(t)
	if err := s.st.Load().SetPublicPreview(t.Context(), "sb1", false); err != nil {
		t.Fatalf("make sb1 private: %v", err)
	}
	tok := s.previewToken(t.Context(), "sb1", 8000)
	if tok == "" {
		t.Fatal("no preview token minted")
	}

	old := hostReq(t, s, "8000-sb1.pilldrops.com", "/page?token="+tok)
	if old.Code != http.StatusPermanentRedirect {
		t.Fatalf("old host = %d %q, want 308", old.Code, old.Body.String())
	}
	loc := old.Header().Get("Location")
	if loc != "https://8000-sb1.pilldrops.app/page?token="+tok {
		t.Fatalf("Location = %q, want the same path and token on pilldrops.app", loc)
	}
	if c := old.Header().Values("Set-Cookie"); len(c) != 0 || strings.Contains(old.Body.String(), "FROM-APP") {
		t.Fatalf("the legacy host set cookies %v or served the sandbox: %q", c, old.Body.String())
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	w := hostReq(t, s, u.Host, u.RequestURI())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/page") {
		t.Fatalf("new host = %d %q, want the app", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.Join(w.Header().Values("Set-Cookie"), "\n"), "pdpv_sb1_8000=") {
		t.Fatal("the new host granted no preview cookie, so the app's sub-resources would be refused")
	}

	bare := hostReq(t, s, "8000-sb1.pilldrops.com", "/page")
	u, err = url.Parse(bare.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	w = hostReq(t, s, u.Host, u.RequestURI())
	if w.Code != http.StatusForbidden || w.Header().Get("Location") != "" {
		t.Fatalf("tokenless bookmark on the new host = %d Location %q, want the private page", w.Code, w.Header().Get("Location"))
	}
}

// navReq is a browser navigation (Sec-Fetch-Dest: document) when dest is set.
func navReq(t *testing.T, s *Server, host, target, dest string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
	req.Host = host
	req.RemoteAddr = "203.0.113.7:1234"
	if dest != "" {
		req.Header.Set("Sec-Fetch-Dest", dest)
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func TestPathFormNavigationGoesToTheHostForm(t *testing.T) {
	s, _ := domainServer(t)

	w := navReq(t, s, "pilldrops.com", "/preview/sb1/8000/a?token=t", "document")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://8000-sb1.pilldrops.com/a?token=t" {
		t.Fatalf("navigation = %d %q, want 302 to the host form", w.Code, w.Header().Get("Location"))
	}
	// API clients and the CLI send no fetch metadata and keep the path form.
	if w := navReq(t, s, "pilldrops.com", "/preview/sb1/8000/a", ""); !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/a") {
		t.Fatalf("API client = %d %q, want the app", w.Code, w.Body.String())
	}
	// The host form arrives rewritten into the path form and must not bounce.
	if w := navReq(t, s, "8000-sb1.pilldrops.com", "/a", "document"); !strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatalf("host form = %d %q, want the app", w.Code, w.Body.String())
	}
	// A customer host is the sandbox's own origin already.
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create domain = %d", w.Code)
	}
	activate(t, s, "app.example.com")
	if w := navReq(t, s, "app.example.com", "/a", "document"); !strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatalf("customer host = %d %q, want the app", w.Code, w.Body.String())
	}
}

// The path form on the legacy apex still proxied sandbox content; a browser
// navigation there goes to the host form on the current domain.
func TestLegacyApexPathFormNavigationGoesToTheCurrentDomain(t *testing.T) {
	s := legacyServer(t)
	w := navReq(t, s, "pilldrops.com", "/preview/sb1/8000/", "document")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://8000-sb1.pilldrops.app/" {
		t.Fatalf("legacy apex navigation = %d %q, want 302 to the current domain's host form", w.Code, w.Header().Get("Location"))
	}
}

// A portal in front rewrites a preview host into the path form and names the
// visitor's host in X-Forwarded-Host. The portal and the control plane switch
// domains at different times, so that host can be under either one; in both
// orders the preview is served, not bounced back to a host the portal
// redirects again, or to one it would answer with the dashboard.
func TestPathFormFromAPortalOnTheOtherDomainIsServed(t *testing.T) {
	viaPortal := func(s *Server, shown string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://x/preview/sb1/8000/a", nil)
		req.Host = "orchestrator:8080"
		req.RemoteAddr = "203.0.113.7:1234"
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("X-Forwarded-Host", shown)
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}

	notMoved, _ := domainServer(t)
	moved := legacyServer(t)
	for _, tc := range []struct {
		name  string
		s     *Server
		shown string
	}{
		{"portal moved, control plane not", notMoved, "8000-sb1.pilldrops.app"},
		{"control plane moved, portal not", moved, "8000-sb1.pilldrops.com"},
		{"control plane moved, portal not, slug host", moved, "myapp.pilldrops.com"},
	} {
		if w := viaPortal(tc.s, tc.shown); !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/a") {
			t.Errorf("%s: %s = %d Location %q, want the app", tc.name, tc.shown, w.Code, w.Header().Get("Location"))
		}
	}
}

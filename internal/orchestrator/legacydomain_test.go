package orchestrator

import (
	"net/http"
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

package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagesDirOverridesEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sdk.html"), []byte("<h1>edited headline</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PagesDirEnv, dir)

	s, err := New("", "docker", "public", false, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sdk", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body != "<h1>edited headline</h1>" {
		t.Errorf("served the embedded page, not the edit: %q", body[:min(80, len(body))])
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

func TestMarketingIsAbsentFromTheBundle(t *testing.T) {
	for _, slug := range []string{"landing", "pricing", "terms", "privacy", "ai-agent-sandbox"} {
		if _, err := distFS.ReadFile("dist/" + slug + ".html"); err == nil {
			t.Errorf("%s.html is compiled into the binary; the hosted service's pages are not part of this repository", slug)
		}
	}
}

func TestAnalyticsInjectedIntoPublicPages(t *testing.T) {
	pdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pdir, "sdk.html"),
		[]byte("<html><head></head><body>agent workstation</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PagesDirEnv, pdir)
	t.Setenv(AnalyticsDomainEnv, "pilldrops.com")

	s, err := New("", "docker", "public", false, "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sdk", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `data-domain="pilldrops.com"`) {
		t.Error("analytics tag not injected into a disk-served page")
	}
	if !strings.Contains(body, "plausible.io/js/script.js") {
		t.Error("default provider script missing")
	}
	if strings.Index(body, "data-domain=") > strings.Index(strings.ToLower(body), "</head>") {
		t.Error("tag injected after </head>")
	}
}

func TestAnalyticsAbsentWhenUnset(t *testing.T) {
	os.Unsetenv(AnalyticsDomainEnv)
	s, err := New("", "docker", "public", false, "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sdk", nil))
	if strings.Contains(rec.Body.String(), "data-domain=") {
		t.Error("injected analytics with none configured")
	}
}

func TestAnalyticsCustomSrc(t *testing.T) {
	pdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pdir, "sdk.html"),
		[]byte("<html><head></head><body>agent workstation</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PagesDirEnv, pdir)
	t.Setenv(AnalyticsDomainEnv, "agent.pilldrops.com")
	t.Setenv(AnalyticsSrcEnv, "https://stats.pilldrops.com/js/script.js")
	s, _ := New("", "docker", "public", false, "")
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sdk", nil))
	if !strings.Contains(rec.Body.String(), "stats.pilldrops.com") {
		t.Error("custom analytics src ignored")
	}
}

func TestAnalyticsNotDoubled(t *testing.T) {
	tag := `<script defer data-domain="x.com" src="s.js"></script>`
	page := []byte(`<html><head><script defer data-domain="x.com" src="s.js"></script></head><body></body></html>`)
	if got := injectAnalytics(page, tag); strings.Count(string(got), "data-domain=") != 1 {
		t.Error("tag injected twice — every pageview would count double")
	}
}

func TestAnalyticsSkipsPagesWithoutHead(t *testing.T) {
	page := []byte("just some text")
	if got := injectAnalytics(page, `<script></script>`); string(got) != "just some text" {
		t.Errorf("mangled a page with no head: %q", got)
	}
}

package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "sf_sess_abc"

func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	s, err := New(testKey, "process", "admin", true, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func TestDocsAndGuidesServed(t *testing.T) {
	mux := newTestMux(t)
	for path, want := range map[string]string{"/docs": "API", "/guides": "Guides"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200 (public page)", path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("GET %s body missing %q", path, want)
		}
	}

	// A stale embed falls back to the legacy redirects.
	s, err := New(testKey, "process", "admin", true, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.docs, s.guides = nil, nil
	mux2 := http.NewServeMux()
	s.Register(mux2)
	for path, want := range map[string]string{"/docs": "/dashboard#/docs", "/guides": "/docs"} {
		rr := httptest.NewRecorder()
		mux2.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusFound || rr.Header().Get("Location") != want {
			t.Fatalf("stale GET %s = %d→%q, want 302→%q", path, rr.Code, rr.Header().Get("Location"), want)
		}
	}
}

func TestDashboardServedPublicly(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/dashboard", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /dashboard = %d, want 200 (public shell)", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Fatal("serving the shell must not set a session cookie")
		}
	}
	if strings.Contains(rr.Body.String(), testKey) {
		t.Fatal("shell leaked the session key")
	}
}

func TestKeyNotEmbeddedInHTML(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: testKey})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("authed GET /dashboard = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, testKey) {
		t.Fatal("session key is embedded in the served HTML — must not be")
	}
	if strings.Contains(body, "__SANDBOXD_KEY__") {
		t.Fatal("placeholder __SANDBOXD_KEY__ left un-replaced")
	}
}

func TestLoginPageServed(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/dashboard/login", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/login = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<form") || !strings.Contains(body, `name="key"`) {
		t.Fatalf("login page missing the key form")
	}
	if strings.Contains(body, testKey) {
		t.Fatal("login page leaks the key")
	}
}

func TestLoginWrongKeyRejected(t *testing.T) {
	mux := newTestMux(t)
	form := url.Values{"key": {"wrong-key"}}
	req := httptest.NewRequest("POST", "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key POST = %d, want 401", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Fatal("wrong key set a session cookie")
		}
	}
}

func TestLoginCorrectKeySetsCookieAndRedirects(t *testing.T) {
	mux := newTestMux(t)
	form := url.Values{"key": {testKey}}
	req := httptest.NewRequest("POST", "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("correct-key POST = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/dashboard" {
		t.Fatalf("post-login Location = %q, want /dashboard", loc)
	}
	var sc *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookie {
			sc = c
		}
	}
	if sc == nil {
		t.Fatal("correct key set no session cookie")
	}
	if sc.Value != testKey {
		t.Fatalf("cookie value = %q, want %q", sc.Value, testKey)
	}
	if !sc.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if sc.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict")
	}
	if !sc.Secure {
		t.Error("session cookie is not Secure over an https (X-Forwarded-Proto) request")
	}
}

func TestRootRedirectsToDashboard(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want 302", rr.Code)
	}
}

// postLogin submits the key form from ip and returns the status.
func postLogin(t *testing.T, mux *http.ServeMux, ip, key string) int {
	t.Helper()
	form := url.Values{"key": {key}}
	req := httptest.NewRequest("POST", "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":51000"
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code
}

func TestLoginRateLimited(t *testing.T) {
	mux := newTestMux(t)

	got429 := false
	for i := 0; i < 20; i++ {
		switch code := postLogin(t, mux, "198.51.100.7", "wrong-key"); code {
		case http.StatusUnauthorized:
		case http.StatusTooManyRequests:
			got429 = true
		default:
			t.Fatalf("attempt %d = %d, want 401 or 429", i, code)
		}
	}
	if !got429 {
		t.Fatal("20 rapid wrong-key attempts from one IP never hit 429")
	}
	if code := postLogin(t, mux, "198.51.100.7", testKey); code != http.StatusTooManyRequests {
		t.Fatalf("throttled IP with correct key = %d, want 429", code)
	}
	if code := postLogin(t, mux, "203.0.113.9", "wrong-key"); code != http.StatusUnauthorized {
		t.Fatalf("fresh IP wrong key = %d, want 401", code)
	}
	if code := postLogin(t, mux, "203.0.113.50", testKey); code != http.StatusFound {
		t.Fatalf("fresh IP correct key = %d, want 302", code)
	}
}

// A self-hosted install is never a storefront: the root is the dashboard, and
// the hosted service's pages are not routes, even with a file for one on disk.
func TestRootIsTheDashboardAndMarketingIsNotRouted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pricing.html"), []byte("<h1>plans</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PagesDirEnv, dir)
	for _, mode := range []string{"user", "admin"} {
		s, err := New(testKey, "process", mode, mode == "admin", "")
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		mux := http.NewServeMux()
		s.Register(mux)
		get := func(path string) *httptest.ResponseRecorder {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			return rr
		}
		if rr := get("/"); rr.Code != http.StatusFound || rr.Header().Get("Location") != "/dashboard" {
			t.Errorf("%s: GET / = %d→%q, want 302→/dashboard", mode, rr.Code, rr.Header().Get("Location"))
		}
		for _, path := range []string{"/pricing", "/terms", "/ai-agent-sandbox", "/robots.txt", "/sitemap.xml"} {
			if rr := get(path); rr.Code == http.StatusOK {
				t.Errorf("%s: GET %s = 200, want no route", mode, path)
			}
		}
	}
}

func TestSandboxDomainInjection(t *testing.T) {
	s, err := New(testKey, "process", "user", false, "pilldrops.com")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !bytes.Contains(s.index, []byte(`sandboxDomain:"pilldrops.com"`)) {
		t.Fatalf("index missing injected sandboxDomain")
	}
	s2, err := New(testKey, "process", "user", false, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if bytes.Contains(s2.index, []byte("__SANDBOXD_DOMAIN__")) {
		t.Fatalf("empty domain must blank the placeholder")
	}
}

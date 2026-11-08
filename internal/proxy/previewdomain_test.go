package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// legacyFixture redirects pilldrops.com to pilldrops.app in front of a router
// that counts what reaches it and would set a cookie if it ever answered.
func legacyFixture(st *store.Store, now *time.Time) (http.Handler, *int) {
	hits := 0
	l := &LegacyDomains{Current: "pilldrops.app", Domains: []string{"pilldrops.com"}, Now: func() time.Time { return *now }}
	if st != nil {
		l.Store = func() *store.Store { return st }
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.SetCookie(w, &http.Cookie{Name: "from-router", Value: "1"})
	})
	return l.Wrap(next), &hits
}

func legacyDo(h http.Handler, method, host, target string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://x"+target, body)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// readCounter records whether anything read the request body.
type readCounter struct {
	r io.Reader
	n int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestLegacyPreviewHostsRedirectKeepingMethodPathAndQuery(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	h, hits := legacyFixture(nil, &now)
	for _, tc := range []struct{ method, host, target, want string }{
		{http.MethodGet, "8000-sb1.pilldrops.com", "/a/b?token=t&x=1", "https://8000-sb1.pilldrops.app/a/b?token=t&x=1"},
		{http.MethodHead, "3000-sb1.PillDrops.com:443", "/", "https://3000-sb1.pilldrops.app/"},
		{http.MethodPost, "myapp.pilldrops.com", "/hook?ptok=p", "https://myapp.pilldrops.app/hook?ptok=p"},
	} {
		body := &readCounter{r: strings.NewReader(`{"event":"push"}`)}
		w := legacyDo(h, tc.method, tc.host, tc.target, body)
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != tc.want {
			t.Errorf("%s %s%s = %d %q, want 308 %q", tc.method, tc.host, tc.target, w.Code, w.Header().Get("Location"), tc.want)
		}
		if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("%s: a legacy host set cookies %v", tc.host, c)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: a cacheable redirect would stop counting uses", tc.host)
		}
		if body.n != 0 {
			t.Errorf("%s: the redirect read %d body bytes; the client resends the body, nothing here may consume it", tc.host, body.n)
		}
	}
	if *hits != 0 {
		t.Fatalf("a legacy preview host reached the router %d times", *hits)
	}
}

func TestLegacyNonPreviewHostsFallThrough(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	h, hits := legacyFixture(nil, &now)
	hosts := []string{
		"pilldrops.com", "www.pilldrops.com", "api.pilldrops.com", "clerk.pilldrops.com",
		"login.pilldrops.com", "docs.pilldrops.com", "status.pilldrops.com", "a.b.pilldrops.com",
		"8000-sb1.pilldrops.app", "8000-sb1.example.org",
	}
	for _, host := range hosts {
		if w := legacyDo(h, http.MethodGet, host, "/", nil); w.Code != http.StatusOK || w.Header().Get("Location") != "" {
			t.Errorf("%s = %d %q, want it untouched", host, w.Code, w.Header().Get("Location"))
		}
	}
	if *hits != len(hosts) {
		t.Fatalf("router hits = %d, want %d", *hits, len(hosts))
	}
}

func TestLegacyDomainExpiresAfterNinetyDaysWithoutUse(t *testing.T) {
	st := store.OpenForTest(t)
	now := time.Unix(1_000_000_000, 0)
	lastUsed := func() int64 {
		t.Helper()
		at, _, err := st.LegacyDomainLastUsed(t.Context(), "pilldrops.com")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return at
	}

	h, _ := legacyFixture(st, &now)
	if w := legacyDo(h, http.MethodGet, "8000-sb1.pilldrops.com", "/", nil); w.Code != http.StatusPermanentRedirect {
		t.Fatalf("first use = %d, want 308", w.Code)
	}
	if got := lastUsed(); got != now.Unix() {
		t.Fatalf("last used = %d, want %d", got, now.Unix())
	}

	// 89 days later, on a fresh replica so no cache answers: still redirecting, and the clock moves.
	now = now.Add(89 * 24 * time.Hour)
	h, _ = legacyFixture(st, &now)
	if w := legacyDo(h, http.MethodGet, "8000-sb1.pilldrops.com", "/", nil); w.Code != http.StatusPermanentRedirect {
		t.Fatalf("use after 89 days = %d, want 308", w.Code)
	}
	if got := lastUsed(); got != now.Unix() {
		t.Fatalf("last used = %d, want %d: a use must push the end out", got, now.Unix())
	}

	// 91 days with no use: the moved page, no redirect, and the visit does not revive it.
	stale := now.Unix()
	now = now.Add(91 * 24 * time.Hour)
	h, hits := legacyFixture(st, &now)
	w := legacyDo(h, http.MethodGet, "8000-sb1.pilldrops.com", "/", nil)
	if w.Code != http.StatusGone || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "pilldrops.app") {
		t.Fatalf("after 91 idle days = %d %q %q, want 410 naming pilldrops.app", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if *hits != 0 || len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("an expired legacy host reached the router or set a cookie")
	}
	if got := lastUsed(); got != stale {
		t.Fatalf("last used = %d, want %d: a visit after expiry revived the domain", got, stale)
	}
}

func TestLegacyUseIsWrittenAtMostOncePerWindow(t *testing.T) {
	st := store.OpenForTest(t)
	now := time.Unix(1_000_000_000, 0)
	h, _ := legacyFixture(st, &now)
	lastUsed := func() int64 {
		t.Helper()
		at, _, err := st.LegacyDomainLastUsed(t.Context(), "pilldrops.com")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return at
	}

	legacyDo(h, http.MethodGet, "8000-sb1.pilldrops.com", "/", nil)
	first := now.Unix()
	now = now.Add(5 * time.Minute)
	legacyDo(h, http.MethodGet, "3000-sb2.pilldrops.com", "/", nil)
	if got := lastUsed(); got != first {
		t.Fatalf("last used = %d five minutes later, want %d: a replica wrote twice in one window", got, first)
	}
	now = now.Add(6 * time.Minute)
	legacyDo(h, http.MethodGet, "8000-sb1.pilldrops.com", "/", nil)
	if got := lastUsed(); got != now.Unix() {
		t.Fatalf("last used = %d eleven minutes later, want %d", got, now.Unix())
	}
}

// The dashboard must sit on a host the redirect leaves alone: the apex or a
// reserved label.
func TestParseLegacyDomainsAcceptsPlatformHostsForTheDashboard(t *testing.T) {
	for _, origin := range []string{"https://pilldrops.com", "https://www.pilldrops.com", "https://app.pilldrops.com"} {
		if _, err := ParseLegacyDomains("pilldrops.com", "pilldrops.app", origin); err != nil {
			t.Errorf("%s: %v", origin, err)
		}
	}
}

func TestLegacyHostError(t *testing.T) {
	legacy := []string{"pilldrops.com"}
	err := LegacyHostError(legacy, "PORTAL_ORCHESTRATOR_URL", "https://console.pilldrops.com:8443/v1")
	if err == nil || !strings.Contains(err.Error(), "PORTAL_ORCHESTRATOR_URL host console.pilldrops.com") ||
		!strings.Contains(err.Error(), "reserve the label \"console\"") {
		t.Fatalf("err = %v, want one naming the setting, the host and the fix", err)
	}
	for _, u := range []string{"https://api.pilldrops.com", "http://orchestrator:8080", "https://pilldrops.com", ""} {
		if err := LegacyHostError(legacy, "PORTAL_ORCHESTRATOR_URL", u); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	if err := LegacyHostError(nil, "PORTAL_ORCHESTRATOR_URL", "https://console.pilldrops.com"); err != nil {
		t.Errorf("no legacy domain: %v", err)
	}
}

func TestParseLegacyDomains(t *testing.T) {
	got, err := ParseLegacyDomains(" PillDrops.com., old.example ,", "pilldrops.app", "https://pilldrops.com")
	if err != nil || len(got) != 2 || got[0] != "pilldrops.com" || got[1] != "old.example" {
		t.Fatalf("parse = %v %v", got, err)
	}
	if got, err := ParseLegacyDomains("", "pilldrops.app", ""); err != nil || got != nil {
		t.Fatalf("unset = %v %v, want nothing and no error", got, err)
	}
	for _, tc := range []struct{ name, raw, current, origin, want string }{
		{"no current domain", "pilldrops.com", "", "https://pilldrops.com", "PORTAL_SANDBOX_DOMAIN"},
		{"same as current", "pilldrops.app", "pilldrops.app", "https://pilldrops.com", "overlaps"},
		{"under current", "old.pilldrops.app", "pilldrops.app", "https://pilldrops.com", "overlaps"},
		{"current under it", "app", "pilldrops.app", "https://pilldrops.com", "overlaps"},
		{"no dashboard origin", "pilldrops.com", "pilldrops.app", "", "PORTAL_ORIGIN"},
		{"dashboard on a slug-shaped host", "pilldrops.com", "pilldrops.app", "https://console.pilldrops.com", "console.pilldrops.com"},
		{"dashboard on a sandbox host", "pilldrops.com", "pilldrops.app", "https://8000-sb1.pilldrops.com", "8000-sb1.pilldrops.com"},
	} {
		if _, err := ParseLegacyDomains(tc.raw, tc.current, tc.origin); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

func TestDashboardOffDomain(t *testing.T) {
	for _, tc := range []struct {
		domain, origin string
		want           bool
	}{
		{"pilldrops.app", "https://pilldrops.com", true},
		{"pilldrops.app", "https://pilldrops.app", false},
		{"example.com", "https://app.example.com", false},
		{"example.com", "", false},
		{"", "https://pilldrops.com", false},
	} {
		if got := DashboardOffDomain(tc.domain, tc.origin); got != tc.want {
			t.Errorf("DashboardOffDomain(%q, %q) = %v, want %v", tc.domain, tc.origin, got, tc.want)
		}
	}
}

func TestSandboxDomainOnlyServesNoDashboard(t *testing.T) {
	hits := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ })
	legacy := &LegacyDomains{Current: "pilldrops.app", Domains: []string{"pilldrops.com"}}
	h := legacy.SandboxDomainOnly("pilldrops.app", "https://pilldrops.com/", next)
	for _, tc := range []struct {
		host, target string
		code         int
		loc          string
	}{
		{"pilldrops.app", "/dashboard?x=1", http.StatusFound, "https://pilldrops.com/dashboard?x=1"},
		{"www.pilldrops.app:443", "/", http.StatusFound, "https://pilldrops.com/"},
		{"nope.pilldrops.app", "/login", http.StatusNotFound, ""},
		{"a.b.pilldrops.app", "/", http.StatusNotFound, ""},
	} {
		w := legacyDo(h, http.MethodGet, tc.host, tc.target, nil)
		if w.Code != tc.code || w.Header().Get("Location") != tc.loc {
			t.Errorf("%s%s = %d %q, want %d %q", tc.host, tc.target, w.Code, w.Header().Get("Location"), tc.code, tc.loc)
		}
	}
	if hits != 0 {
		t.Fatalf("a non-preview host under the preview domain reached the dashboard %d times", hits)
	}

	// A host a stage resolved to a sandbox, and the dashboard's own host, pass.
	r := httptest.NewRequest(http.MethodGet, "http://x/preview/sb1/8000/", nil)
	r.Host = "8000-sb1.pilldrops.app"
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(WithPreviewHost(r.Context())))
	legacyDo(h, http.MethodGet, "pilldrops.com", "/dashboard", nil)
	if hits != 2 {
		t.Fatalf("hits = %d, want the resolved preview and the dashboard host through", hits)
	}

	// The dashboard on the sandbox domain itself, the old shape, is untouched.
	h = legacy.SandboxDomainOnly("pilldrops.app", "", next)
	legacyDo(h, http.MethodGet, "pilldrops.app", "/dashboard", nil)
	if hits != 3 {
		t.Fatal("with no PORTAL_ORIGIN the apex must still serve the dashboard")
	}

	// With no legacy domain nothing moved, so nothing changes either.
	for _, none := range []*LegacyDomains{nil, {Current: "pilldrops.app"}} {
		h = none.SandboxDomainOnly("pilldrops.app", "https://pilldrops.com", next)
		legacyDo(h, http.MethodGet, "pilldrops.app", "/dashboard", nil)
		legacyDo(h, http.MethodGet, "nope.pilldrops.app", "/", nil)
	}
	if hits != 7 {
		t.Fatalf("hits = %d, want every request through with no legacy domain", hits)
	}
}

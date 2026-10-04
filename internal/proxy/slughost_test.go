package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
)

// slugResolver is a stubResolver that also answers BySlug from a fixed map.
type slugResolver struct {
	stubResolver
	slugs map[string]struct {
		id   string
		port int
	}
}

func (s *slugResolver) BySlug(_ context.Context, slug string) (string, int, error) {
	t, ok := s.slugs[slug]
	if !ok {
		return "", 0, errors.New("no such slug")
	}
	return t.id, t.port, nil
}

func TestParseSlugHost(t *testing.T) {
	const d = "sb.pilldrops.com"
	for _, c := range []struct {
		host string
		want string
		ok   bool
	}{
		{"myapp." + d, "myapp", true},
		{"w1-api." + d, "w1-api", true},
		{"8000-sb1." + d, "", false},  // port form
		{"agent-sb1." + d, "", false}, // agent- prefix stays reserved in ValidSlug
		{"www." + d, "", false},       // reserved
		{"MyApp." + d, "", false},     // callers lower-case first; uppercase is not a slug
		{"a.b." + d, "", false},
		{d, "", false},
		{"myapp.other.com", "", false},
	} {
		got, ok := ParseSlugHost(c.host, d)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseSlugHost(%q) = (%q,%v), want (%q,%v)", c.host, got, ok, c.want, c.ok)
		}
	}
	if _, ok := ParseSlugHost("myapp.x", ""); ok {
		t.Error("empty domain must never match")
	}
}

func slugRouter(t *testing.T, upstream string, next http.Handler) (*slugResolver, http.Handler) {
	t.Helper()
	res := &slugResolver{stubResolver: stubResolver{target: upstream}, slugs: map[string]struct {
		id   string
		port int
	}{"myapp": {"sb-1783-abcd", 3000}}}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	return res, HostRouter("sb.pilldrops.com", res, next, verify)
}

func TestSlugHostRoutesToTheHoldersIDAndPort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	res, h := slugRouter(t, strings.TrimPrefix(upstream.URL, "http://"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a held slug must not fall through to next")
	}))

	tok := previewtoken.Token([]byte(hostGateSecret), "sb-1783-abcd", 3000, "u:cust1")
	req := httptest.NewRequest("GET", "http://x/index.html?ptok="+tok, nil)
	req.Host = "myapp.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if res.gotID != "sb-1783-abcd" || res.gotPort != 3000 {
		t.Fatalf("resolver got (%q,%d), want (sb-1783-abcd,3000): the slug's public port", res.gotID, res.gotPort)
	}
	if body := rec.Body.String(); body != "app-saw:/index.html" {
		t.Fatalf("body = %q", body)
	}

	// The id form still routes by id.
	tok = previewtoken.Token([]byte(hostGateSecret), "sb1", 8000, "u:cust1")
	req = httptest.NewRequest("GET", "http://x/?ptok="+tok, nil)
	req.Host = "8000-sb1.sb.pilldrops.com"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if res.gotID != "sb1" || res.gotPort != 8000 {
		t.Fatalf("id form resolved (%q,%d), want (sb1,8000)", res.gotID, res.gotPort)
	}
}

func TestSlugHostTokenStillBindsToTheID(t *testing.T) {
	_, h := slugRouter(t, "127.0.0.1:1", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a refused slug preview must not fall through")
	}))
	// A token minted for a different sandbox (same org, same port) is refused.
	tok := previewtoken.Token([]byte(hostGateSecret), "sb-other", 3000, "u:cust1")
	req := httptest.NewRequest("GET", "http://x/?ptok="+tok, nil)
	req.Host = "myapp.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-id token on the slug host = %d, want 403", rec.Code)
	}
	// And a token for the holder but the wrong port is refused too.
	tok = previewtoken.Token([]byte(hostGateSecret), "sb-1783-abcd", 8000, "u:cust1")
	req = httptest.NewRequest("GET", "http://x/?ptok="+tok, nil)
	req.Host = "myapp.sb.pilldrops.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-port token on the slug host = %d, want 403", rec.Code)
	}
}

func TestUnknownSlugFallsThrough(t *testing.T) {
	nextHit := ""
	_, h := slugRouter(t, "127.0.0.1:1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextHit = r.URL.Path }))

	req := httptest.NewRequest("GET", "http://x/site", nil)
	req.Host = "nope.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if nextHit != "/site" {
		t.Fatalf("unknown slug did not reach next (path %q, status %d)", nextHit, rec.Code)
	}

	// agent-sb1 is also an unknown label now (no agent-host routing left);
	// it must refuse rather than reach next or any rewritten path.
	req = httptest.NewRequest("GET", "http://x/v1/agent/status", nil)
	req.Host = "agent-sb1.sb.pilldrops.com"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agent-sb1 host = %d, want 404", rec.Code)
	}

	// A resolver without BySlug keeps the old 404 for a non-id label.
	plain := HostRouter("sb.pilldrops.com", &stubResolver{}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("without a SlugResolver a slug-shaped host must not reach next")
	}), alwaysAllowPreview)
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "myapp.sb.pilldrops.com"
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no SlugResolver: status = %d, want 404", rec.Code)
	}
}

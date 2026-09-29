package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
)

// alwaysAllowPreview is the verifyPreview stub for tests about host parsing
// rather than the token gate.
func alwaysAllowPreview(string, int, string) (string, bool) { return stubOrg, true }

const stubOrg = "u:cust1"

func TestParseSandboxHost(t *testing.T) {
	const domain = "sb.pilldrops.com"
	cases := []struct {
		name    string
		host    string
		wantID  string
		wantPrt int
		wantOK  bool
	}{
		{"valid hyphenated id", "8000-sb-1783-abcd.sb.pilldrops.com", "sb-1783-abcd", 8000, true},
		{"valid simple id", "3000-sbxyz.sb.pilldrops.com", "sbxyz", 3000, true},
		{"max port", "65535-sb-x.sb.pilldrops.com", "sb-x", 65535, true},
		{"not our domain", "8000-sb-x.other.com", "", 0, false},
		{"apex only", "sb.pilldrops.com", "", 0, false},
		{"deeper subdomain", "8000-sb-x.extra.sb.pilldrops.com", "", 0, false},
		{"missing port", "sb-1783-abcd.sb.pilldrops.com", "", 0, false},
		{"non-numeric port", "abc-sb-x.sb.pilldrops.com", "", 0, false},
		{"port too large", "70000-sb-x.sb.pilldrops.com", "", 0, false},
		{"zero port", "0-sb-x.sb.pilldrops.com", "", 0, false},
		{"reserved label www", "www.sb.pilldrops.com", "", 0, false},
		{"empty id after port", "8000-.sb.pilldrops.com", "", 0, false},

		// The id becomes a URL path segment downstream, so a slash would make
		// two components disagree about which sandbox is named.
		{"slash in id", "8000-a/b.sb.pilldrops.com", "", 0, false},
		{"backslash in id", `8000-a\b.sb.pilldrops.com`, "", 0, false},
		{"percent-encoded traversal", "8000-%2e%2e%2fadmin.sb.pilldrops.com", "", 0, false},
		{"null byte", "8000-sb1\x00.sb.pilldrops.com", "", 0, false},
		{"carriage return", "8000-sb1\r.sb.pilldrops.com", "", 0, false},
		{"newline", "8000-sb1\n.sb.pilldrops.com", "", 0, false},
		{"space", "8000-sb 1.sb.pilldrops.com", "", 0, false},
		{"colon", "8000-sb:1.sb.pilldrops.com", "", 0, false},
		{"at sign", "8000-sb@1.sb.pilldrops.com", "", 0, false},
		{"underscore", "8000-sb_1.sb.pilldrops.com", "", 0, false},
		{"non-ascii", "8000-sbé1.sb.pilldrops.com", "", 0, false},
		// IDN arrives as punycode, which is plain ASCII.
		{"punycode id is still accepted", "8000-xn--sb1-9na.sb.pilldrops.com", "xn--sb1-9na", 8000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, port, ok := ParseSandboxHost(c.host, domain)
			if ok != c.wantOK || id != c.wantID || port != c.wantPrt {
				t.Fatalf("ParseSandboxHost(%q) = (%q,%d,%v), want (%q,%d,%v)",
					c.host, id, port, ok, c.wantID, c.wantPrt, c.wantOK)
			}
		})
	}
}

func TestParseSandboxHost_EmptyDomain(t *testing.T) {
	if _, _, ok := ParseSandboxHost("8000-x.sb.pilldrops.com", ""); ok {
		t.Fatal("empty domain must never match")
	}
}

func TestHostRouter_ProxiesSandboxHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	res := &stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")}

	nextHit := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextHit = true })
	h := HostRouter("sb.pilldrops.com", res, next, alwaysAllowPreview)

	req := httptest.NewRequest("GET", "http://x/some/path", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if nextHit {
		t.Fatal("sandbox host must not fall through to next")
	}
	if res.gotID != "sb-1783-abcd" || res.gotPort != 8000 {
		t.Fatalf("resolver got (%q,%d), want (sb-1783-abcd,8000)", res.gotID, res.gotPort)
	}
	if body := rec.Body.String(); body != "app-saw:/some/path" {
		t.Fatalf("body = %q, want app-saw:/some/path", body)
	}
}

const hostGateSecret = "test-host-gate-secret"

// verifyPreviewLikeServer mirrors (*httpapi.Server).VerifyPreview without
// importing httpapi, which would be an import cycle.
func verifyPreviewLikeServer(secret []byte, org string) func(id string, port int, token string) (string, bool) {
	return func(id string, port int, token string) (string, bool) {
		if !previewtoken.Authorized(secret, token, id, port, org, false) {
			return "", false
		}
		return org, true
	}
}

func TestHostRouter_SubdomainPreviewRequiresToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	res := &stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("refused subdomain preview must not fall through to next")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/index.html", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("no ?ptok=: status = %d, want 403", rec.Code)
	}
	if res.gotID != "" {
		t.Fatal("PreviewTarget was called before the token was checked")
	}
}

func TestHostRouter_SubdomainPreviewWrongTokenRefused(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("refused subdomain preview must not fall through to next")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/?ptok=not-a-real-token", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong ?ptok=: status = %d, want 403", rec.Code)
	}
}

func TestHostRouter_SubdomainPreviewCrossOrgRefused(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	tok := previewtoken.Token([]byte(hostGateSecret), "sb-1783-abcd", 8000, "u:other-org")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("cross-org token must not reach next")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/?ptok="+tok, nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-org ?ptok=: status = %d, want 403", rec.Code)
	}
}

func TestHostRouter_SubdomainPreviewValidOrgBoundTokenProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	res := &stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	tok := previewtoken.Token([]byte(hostGateSecret), "sb-1783-abcd", 8000, "u:cust1")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("valid preview must not fall through to next")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/index.html?ptok="+tok, nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid org-bound ?ptok=: status = %d, want 200", rec.Code)
	}
	if res.gotID != "sb-1783-abcd" || res.gotPort != 8000 {
		t.Fatalf("resolver got (%q,%d), want (sb-1783-abcd,8000)", res.gotID, res.gotPort)
	}
}

// The ?ptok= in a shared preview URL must not reach a third party via Referer.
func TestHostRouter_SubdomainPreviewCarriesNoReferrer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	res := &stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")}
	verify := verifyPreviewLikeServer([]byte(hostGateSecret), "u:cust1")
	tok := previewtoken.Token([]byte(hostGateSecret), "sb-1783-abcd", 8000, "u:cust1")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("valid preview must not fall through to next")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/index.html?ptok="+tok, nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer — this URL's ?ptok= is a "+
			"working preview credential for the life of the token", got)
	}
}

func TestHostRouter_SubdomainPreviewFailsClosedWhenSecretUnset(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	verify := verifyPreviewLikeServer(nil, "u:cust1")
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not fall through to next with no secret configured")
	}), verify)

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("no secret, no dev hatch: status = %d, want 403 (refused)", rec.Code)
	}
}

func TestHostRouter_SubdomainPreviewNilVerifyFailsClosed(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("nil verifyPreview must not silently allow through")
	}), nil)

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("nil verifyPreview: status = %d, want 403 (fail closed)", rec.Code)
	}
}

func TestHostRouter_FallsThroughNonSandboxHost(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	nextHit := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextHit = true })
	h := HostRouter("sb.pilldrops.com", res, next, alwaysAllowPreview)

	req := httptest.NewRequest("GET", "http://x/dashboard", nil)
	req.Host = "app.pilldrops.com"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !nextHit {
		t.Fatal("non-sandbox host must fall through to next")
	}
}

func TestHostRouter_MalformedSandboxHostIs404(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("malformed host under the domain must not reach next")
	})
	h := HostRouter("sb.pilldrops.com", res, next, alwaysAllowPreview)

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "www.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHostRouter_EmptyDomainReturnsNext(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if got := HostRouter("", &stubResolver{}, next, alwaysAllowPreview); got == nil {
		t.Fatal("empty domain must return a non-nil handler (next)")
	}
}

func TestParseAgentHost(t *testing.T) {
	const domain = "sb.pilldrops.com"
	cases := []struct {
		name   string
		host   string
		wantID string
		wantOK bool
	}{
		{"simple id", "agent-sb1.sb.pilldrops.com", "sb1", true},
		{"hyphenated id", "agent-sb-1783-abcd.sb.pilldrops.com", "sb-1783-abcd", true},
		{"id beginning with agent-", "agent-agent-x.sb.pilldrops.com", "agent-x", true},
		{"not our domain", "agent-sb1.other.com", "", false},
		{"deeper subdomain", "agent-sb1.extra.sb.pilldrops.com", "", false},
		{"apex", "sb.pilldrops.com", "", false},
		{"empty id", "agent-.sb.pilldrops.com", "", false},
		{"bare label", "agent.sb.pilldrops.com", "", false},
		{"different label", "agents-sb1.sb.pilldrops.com", "", false},
		{"a port host is not an agent host", "8000-sb1.sb.pilldrops.com", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := ParseAgentHost(c.host, domain)
			if ok != c.wantOK || id != c.wantID {
				t.Fatalf("ParseAgentHost(%q) = (%q,%v), want (%q,%v)",
					c.host, id, ok, c.wantID, c.wantOK)
			}
		})
	}
}

func TestParseAgentHost_EmptyDomain(t *testing.T) {
	if _, ok := ParseAgentHost("agent-sb1.sb.pilldrops.com", ""); ok {
		t.Fatal("empty domain must never match")
	}
}

func TestAgentHostIsCheckedBeforeThePortParse(t *testing.T) {
	if _, _, ok := ParseSandboxHost("agent-sb1.sb.pilldrops.com", "sb.pilldrops.com"); ok {
		t.Fatal("parseSandboxHost accepted an agent host; the two must not overlap")
	}
	res := &stubResolver{target: "127.0.0.1:1"}
	var sawPath string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawPath = r.URL.Path })
	h := HostRouter("sb.pilldrops.com", res, next, alwaysAllowPreview)

	req := httptest.NewRequest("GET", "http://x/v1/agent/status", nil)
	req.Host = "agent-sb1.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("agent host 404'd: %s", rec.Body.String())
	}
	if sawPath != "/v1/sandboxes/sb1/agent/status" {
		t.Fatalf("mux saw %q, want the rewritten node route", sawPath)
	}
	if res.gotID != "" {
		t.Fatalf("the request was proxied to the sandbox (%q) instead of served by the node", res.gotID)
	}
}

func TestAgentRequestPathRewritesOnlyWhatItShould(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"/v1/agent/status", "/v1/sandboxes/sb1/agent/status", true},
		{"/v1/agent/turns/t1/interrupt", "/v1/sandboxes/sb1/agent/turns/t1/interrupt", true},
		{"/v1/agent/events", "/v1/sandboxes/sb1/agent/events", true},
		{"/agent/claim", "/agent/claim", true},
		{"/v1/sandboxes", "", false},
		{"/v1/sandboxes/sb1/exec", "", false},
		{"/v1/keys", "", false},
		{"/dashboard", "", false},
		{"/", "", false},
		// A different sandbox in the path must not outrank the hostname.
		{"/v1/sandboxes/other/agent/status", "", false},
	} {
		got, ok := AgentRequestPath("sb1", c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("AgentRequestPath(sb1, %q) = (%q,%v), want (%q,%v)",
				c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAgentHostRefusesNonAgentPaths(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("a non-agent path reached the node mux: %s", r.URL.Path)
	})
	h := HostRouter("sb.pilldrops.com", &stubResolver{}, next, alwaysAllowPreview)

	for _, p := range []string{"/v1/sandboxes", "/v1/keys", "/dashboard", "/"} {
		req := httptest.NewRequest("GET", "http://x"+p, nil)
		req.Host = "agent-sb1.sb.pilldrops.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on the agent origin = %d, want 404", p, rec.Code)
		}
	}
}

func TestAppPreviewStillWorksAlongsideTheAgentOrigin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app-saw:"+r.URL.Path)
	}))
	defer upstream.Close()
	res := &stubResolver{target: strings.TrimPrefix(upstream.URL, "http://")}
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("an app preview host must not fall through to the mux")
	}), alwaysAllowPreview)

	// An id beginning with "agent-" is the case a substring check would steal.
	req := httptest.NewRequest("GET", "http://x/index.html", nil)
	req.Host = "8000-agent-sb1.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if res.gotID != "agent-sb1" || res.gotPort != 8000 {
		t.Fatalf("resolver got (%q,%d), want (agent-sb1,8000)", res.gotID, res.gotPort)
	}
	if body := rec.Body.String(); body != "app-saw:/index.html" {
		t.Fatalf("body = %q", body)
	}
}

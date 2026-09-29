package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

const originDomain = "pilldrops.com"

// originSandboxID is the sandbox every agent-host fixture names.
const originSandboxID = "sb1"

// mountAgentOrigin mounts the agent origin in front of a recording site router.
func mountAgentOrigin(t *testing.T, orchURL string) (h http.Handler, siteHits *int) {
	t.Helper()
	hits := 0
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("the marketing site"))
	})
	s := NewServer("fleet-key", "nodetoken")
	upsert(s.reg, "n1", orchURL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	s.reg.Place(originSandboxID, "n1")
	return s.agentOrigin(originDomain, site), &hits
}

func originReq(method, host, path string) *http.Request {
	r := httptest.NewRequest(method, "http://x"+path, nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.7:1234"
	return r
}

// fakeOrch stands in for the node hop.
func fakeOrch(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func TestAgentOriginRoutesInsteadOfServingTheSite(t *testing.T) {
	var gotPath string
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"state":"idle"}`))
	})
	h, siteHits := mountAgentOrigin(t, orch)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status"))

	if *siteHits != 0 {
		t.Fatal("the agent origin fell through to the site router")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/sandboxes/sb1/agent/status" {
		t.Fatalf("control plane saw %q, want the sandbox-scoped route", gotPath)
	}
}

func TestOtherHostsPassThroughUntouched(t *testing.T) {
	h, siteHits := mountAgentOrigin(t, "http://unused")

	for _, host := range []string{
		originDomain,                     // apex
		"www." + originDomain,            // www
		"app." + originDomain,            // some other label
		"agent-sb1.example.org",          // right label, wrong domain
		"agent." + originDomain,          // bare label, no sandbox
		"agent-sb1.deep." + originDomain, // deeper subdomain
	} {
		*siteHits = 0
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, originReq("GET", host, "/"))
		if *siteHits != 1 {
			t.Errorf("host %q did not reach the site router (status %d)", host, rr.Code)
		}
	}
}

// Unknown paths must 404, not fall through to the portal router with the recipient's cookies.
func TestAgentOriginIsNotASecondFrontDoor(t *testing.T) {
	h, siteHits := mountAgentOrigin(t, "http://unused")

	for _, p := range []string{
		"/v1/sandboxes",
		"/v1/sandboxes/other/agent/status", // a DIFFERENT sandbox named in the path
		"/v1/sandboxes/sb1/exec",
		"/v1/keys",
		"/v1/limits",
		"/dashboard",
		"/dashboard/api/logs",
	} {
		*siteHits = 0
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, p))
		if *siteHits != 0 {
			t.Errorf("%s reached the portal router from the agent origin", p)
		}
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, rr.Code)
		}
	}
}

func TestTheHostnameOutranksThePath(t *testing.T) {
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("a cross-sandbox path was proxied: %s", r.URL.Path)
	})
	h, _ := mountAgentOrigin(t, orch)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/v1/sandboxes/victim/agent/events"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestBearerAATIsLiftedIntoTheRelayHeader(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status")
	req.Header.Set("Authorization", "Bearer sfa_abc123_"+strings.Repeat("d", 32))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get(agentTokenHeader) == "" {
		t.Fatal("the AAT was dropped; the node would see only the operator key")
	}
	if !strings.HasPrefix(got.Get(agentTokenHeader), "sfa_") {
		t.Fatalf("relayed token = %q", got.Get(agentTokenHeader))
	}
	if a := got.Get("Authorization"); strings.Contains(a, "sfa_") {
		t.Fatalf("the caller's AAT survived in Authorization: %q", a)
	} else if a != "Bearer kk" {
		t.Fatalf("Authorization = %q, want the node credential", a)
	}
}

// EventSource cannot set headers, so the cookie is the only credential a streaming viewer has.
func TestCookieAATIsLifted(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/events")
	req.AddCookie(&http.Cookie{Name: agentCookieName, Value: "sfa_abc123_" + strings.Repeat("d", 32)})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get(agentTokenHeader) == "" {
		t.Fatal("the cookie credential was dropped; an EventSource viewer cannot authenticate")
	}
	if got.Get("Cookie") != "" {
		t.Fatalf("cookies were forwarded onward: %q", got.Get("Cookie"))
	}
}

func TestInboundRelayHeaderIsAlwaysStripped(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status")
	req.Header.Set(agentTokenHeader, "sfa_forged_"+strings.Repeat("e", 32))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get(agentTokenHeader) != "" {
		t.Fatalf("a client-asserted relay header survived: %q", got.Get(agentTokenHeader))
	}
}

func TestNonAATBearerIsNotLifted(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status")
	req.Header.Set("Authorization", "Bearer sf_live_orgkey_abcdef")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get(agentTokenHeader) != "" {
		t.Fatalf("a non-AAT credential was lifted into the agent header: %q",
			got.Get(agentTokenHeader))
	}
}

func TestNoOrgOverrideOnTheAgentOrigin(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	h.ServeHTTP(httptest.NewRecorder(), originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status"))

	if v := got.Get("X-Org-Override"); v != "" {
		t.Fatalf("X-Org-Override = %q on an origin with no portal identity", v)
	}
}

func TestForwardedHeadersAreSetNotRelayed(t *testing.T) {
	var got http.Header
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status")
	req.Header.Set("X-Forwarded-Host", "attacker.example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get("X-Forwarded-Host") != "agent-sb1."+originDomain {
		t.Fatalf("X-Forwarded-Host = %q, want the real agent host", got.Get("X-Forwarded-Host"))
	}
	if got.Get("X-Forwarded-Proto") == "" {
		t.Fatal("X-Forwarded-Proto unset: the claim cookie would lose its Secure flag")
	}
}

func TestClaimIsRewrittenToTheRoutableForm(t *testing.T) {
	var gotPath, gotQuery string
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusSeeOther)
	})
	h, _ := mountAgentOrigin(t, orch)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/agent/claim?code=abc123"))

	if gotPath != "/v1/sandboxes/sb1/agent/claim" {
		t.Fatalf("claim proxied to %q", gotPath)
	}
	if gotQuery != "code=abc123" {
		t.Fatalf("claim query = %q; the code would be lost", gotQuery)
	}
}

func TestViewerIsServedAtTheRoot(t *testing.T) {
	h, siteHits := mountAgentOrigin(t, "http://unused")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/"))

	if *siteHits != 0 {
		t.Fatal("the root of the agent origin served the marketing site")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "sb1") {
		t.Error("the viewer does not name the sandbox it is showing")
	}
	if !strings.Contains(body, "/v1/agent/events") {
		t.Error("the viewer does not subscribe to the event stream")
	}
	if !strings.Contains(body, "Read-only") {
		t.Error("the viewer does not say it is read-only")
	}
}

func TestViewerEscapesTheSandboxIdFromTheHost(t *testing.T) {
	h, _ := mountAgentOrigin(t, "http://unused")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", `agent-<script>alert(1)</script>.`+originDomain, "/"))

	body := rr.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("the sandbox id from the Host header was injected into the page unescaped")
	}
}

func TestCSPConfinesAgentOutput(t *testing.T) {
	h, _ := mountAgentOrigin(t, "http://unused")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/"))

	csp := rr.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"connect-src 'self'",     // no exfiltration to another host
		"frame-ancestors 'none'", // never framed inside the dashboard
		"form-action 'none'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if rr.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("X-Frame-Options is not DENY")
	}
	if rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("Referrer-Policy is not no-referrer")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("the viewer is cacheable")
	}
}

func TestCORSIsSameOriginOnly(t *testing.T) {
	h, _ := mountAgentOrigin(t, "http://unused")

	same := httptest.NewRecorder()
	req := originReq("GET", "agent-sb1."+originDomain, "/")
	req.Header.Set("Origin", "https://agent-sb1."+originDomain)
	h.ServeHTTP(same, req)
	if same.Header().Get("Access-Control-Allow-Origin") != "https://agent-sb1."+originDomain {
		t.Errorf("same-origin request got ACAO %q", same.Header().Get("Access-Control-Allow-Origin"))
	}
	if same.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("credentials not allowed for the origin's own page")
	}

	for _, bad := range []string{
		"https://evil.example.com",
		"https://" + originDomain,             // the dashboard itself
		"https://agent-other." + originDomain, // a different sandbox's origin
	} {
		rr := httptest.NewRecorder()
		req := originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status")
		req.Header.Set("Origin", bad)
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q was granted CORS access (%q)", bad, got)
		}
	}
}

func TestUnauthenticatedOriginIsRateLimited(t *testing.T) {
	var hops int
	orch := fakeOrch(t, func(w http.ResponseWriter, r *http.Request) {
		hops++
		_, _ = w.Write([]byte(`{}`))
	})
	h, _ := mountAgentOrigin(t, orch)

	limited := false
	for range agentOriginBurst + 20 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, originReq("GET", "agent-sb1."+originDomain, "/v1/agent/status"))
		if rr.Code == http.StatusTooManyRequests {
			limited = true
			if rr.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After: a client cannot tell how long to wait")
			}
			break
		}
	}
	if !limited {
		t.Fatal("no rate limit on an unauthenticated origin that costs two internal hops per request")
	}
	// A refused request must not have cost a hop.
	if hops > agentOriginBurst {
		t.Fatalf("%d internal hops for %d allowed requests", hops, agentOriginBurst)
	}
}

func TestIPLimiterRefillsOverTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newIPLimiter(5, 10)
	l.now = func() time.Time { return now }

	for i := range 10 {
		if !l.allow("1.2.3.4") {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("the bucket did not empty")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("a second client was refused by the first client's bucket")
	}
	now = now.Add(time.Second) // 5 tokens back
	for i := range 5 {
		if !l.allow("1.2.3.4") {
			t.Fatalf("refilled request %d refused", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("refill exceeded the rate")
	}
}

// An unplaced sandbox must 404, not fall through to the site router.
func TestAgentOriginForAnUnknownSandboxIs404(t *testing.T) {
	s := NewServer("fleet-key", "nodetoken")
	h := s.agentOrigin(originDomain, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an unplaced agent request fell through to the site router")
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, originReq("GET", "agent-nosuchbox."+originDomain, "/v1/agent/status"))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestNoDomainDisablesTheOriginEntirely(t *testing.T) {
	hits := 0
	s := NewServer("fleet-key", "nodetoken")
	h := s.agentOrigin("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))

	h.ServeHTTP(httptest.NewRecorder(), originReq("GET", "agent-sb1.anything.com", "/v1/agent/status"))
	if hits != 1 {
		t.Fatal("an unconfigured domain still routed an agent origin")
	}
}

// The agent origin is a second way into a sandbox, and the org's source-network
// restriction did not cover it: peerAllowed had one caller, the preview gate.
// A restricted org's agent API answered any network, share link and all.
func TestAgentOriginAppliesTheOrgSourceRestriction(t *testing.T) {
	s, _ := domainServer(t) // sb1 lives in org u:one, placed on a fake node
	trustProxies(t)
	h := s.agentOrigin(domainTestDomain, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("an agent-origin request fell through to the site router")
	}))
	serve := func(remoteAddr string) *httptest.ResponseRecorder {
		t.Helper()
		req := originReq("GET", "agent-sb1."+domainTestDomain, "/v1/agent/status")
		req.RemoteAddr = remoteAddr
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Positive control: unrestricted, the origin reaches the node.
	if rr := serve("203.0.113.7:1234"); rr.Code == http.StatusNotFound {
		t.Fatalf("with no restriction the agent origin already 404s: %q", rr.Body.String())
	}
	if err := s.st.Load().SetOrgPreviewSources(t.Context(), "u:one", []string{"100.64.0.0/10"}); err != nil {
		t.Fatalf("set sources: %v", err)
	}
	if rr := serve("203.0.113.7:1234"); rr.Code != http.StatusNotFound {
		t.Errorf("off-network peer on the agent origin = %d %q, want the unknown-sandbox 404",
			rr.Code, rr.Body.String())
	}
	if rr := serve("100.64.1.2:1234"); rr.Code == http.StatusNotFound {
		t.Errorf("on-network peer was refused too: %d %q", rr.Code, rr.Body.String())
	}
}

// A restriction that cannot discriminate looks exactly like one that works:
// with no trusted proxy, every request behind a terminator presents the
// terminator's address. The operator is told, at the moment they set it.
func TestSourceRestrictionWarnsWithoutATrustedProxy(t *testing.T) {
	s, _ := domainServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("PUT", "/v1/admin/orgs/u:one/preview-sources", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fleet-key")
		req.RemoteAddr = "203.0.113.7:1234"
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, req)
		return rr
	}

	trustProxies(t) // none
	if got := put(`{"cidrs":["100.64.0.0/10"]}`).Body.String(); !strings.Contains(got, "TRUSTED_PROXY_CIDRS") {
		t.Errorf("setting a restriction with no trusted proxy said nothing: %q", got)
	}
	// Clearing one is not a misconfiguration, and neither is a configured proxy.
	if got := put(`{"cidrs":[]}`).Body.String(); strings.Contains(got, "TRUSTED_PROXY_CIDRS") {
		t.Errorf("clearing the restriction warned anyway: %q", got)
	}
	trustProxies(t, "192.0.2.0/24")
	if got := put(`{"cidrs":["100.64.0.0/10"]}`).Body.String(); strings.Contains(got, "TRUSTED_PROXY_CIDRS") {
		t.Errorf("warned with a trusted proxy configured: %q", got)
	}
}

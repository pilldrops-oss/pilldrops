package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const domainTestDomain = "pilldrops.com"

// domainServer is an orchestrator with a real SQLite store, one fake node and
// one public sandbox (sb1, org u:one) placed on it.
func domainServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("FROM-APP " + r.URL.Path))
	}))
	t.Cleanup(node.Close)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "domains.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := NewServer("fleet-key", "nodetoken")
	s.sandboxDomain = domainTestDomain
	s.SetStore(st)
	if err := upsert(s.reg, "n1", node.URL, "kk", "", nil, "", cluster.Capacity{}, s.now()); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, sb := range []struct{ id, org string }{{"sb1", "u:one"}, {"sb2", "u:two"}} {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: sb.id, OrgID: sb.org, Template: "base", State: "running", PublicPort: 8000,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("save %s: %v", sb.id, err)
		}
		if err := st.SetPublicPreview(t.Context(), sb.id, true); err != nil {
			t.Fatalf("public %s: %v", sb.id, err)
		}
		s.reg.Place(sb.id, "n1")
	}
	return s, node
}

// orgKey mints a real customer API key for an org. Customer keys, not the
// operator key with an override: an operator owns every sandbox by definition,
// so tenancy tested through one would prove nothing.
func orgKey(t *testing.T, s *Server, org string) string {
	t.Helper()
	_, plaintext, err := s.st.Load().CreateKey(t.Context(), org+" key", org)
	if err != nil {
		t.Fatalf("key for %s: %v", org, err)
	}
	return plaintext
}

// apiReq drives the full router as one org's customer.
func apiReq(t *testing.T, s *Server, method, org, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+orgKey(t, s, org))
	req.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

// hostReq is an anonymous browser request arriving on a hostname.
func hostReq(t *testing.T, s *Server, host, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
	req.Host = host
	req.RemoteAddr = "203.0.113.7:1234"
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func jsonField(t *testing.T, w *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// activate stands in for the verifier having seen the DNS record.
func activate(t *testing.T, s *Server, host string) {
	t.Helper()
	if err := s.st.Load().SetDomainStatus(t.Context(), host, "active", "", s.now().Unix()); err != nil {
		t.Fatalf("activate %s: %v", host, err)
	}
	s.domains.forget(host)
}

func TestDomainCreateRefusesAPrivateSandbox(t *testing.T) {
	s, _ := domainServer(t)
	if err := s.st.Load().SetPublicPreview(t.Context(), "sb1", false); err != nil {
		t.Fatalf("make private: %v", err)
	}

	w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("private sandbox = %d, want 409 (body %q)", w.Code, w.Body.String())
	}
	if code := jsonField(t, w, "code"); code != "sandbox_not_public" {
		t.Errorf("code = %q, want sandbox_not_public", code)
	}
	if msg := jsonField(t, w, "error"); !strings.Contains(msg, "public") {
		t.Errorf("refusal %q does not say the sandbox must be public", msg)
	}

	// The same sandbox, flipped public, is accepted.
	if err := s.st.Load().SetPublicPreview(t.Context(), "sb1", true); err != nil {
		t.Fatalf("make public: %v", err)
	}
	w2 := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("public sandbox = %d, want 201 (body %q)", w2.Code, w2.Body.String())
	}
	if got, want := jsonField(t, w2, "cname_target"), "8000-sb1."+domainTestDomain; got != want {
		t.Errorf("cname_target = %q, want %q", got, want)
	}
	if st := jsonField(t, w2, "status"); st != "pending" {
		t.Errorf("status = %q, want pending: nothing routes before verification", st)
	}
}

func TestDomainCreateRefusesPlatformPorts(t *testing.T) {
	s, _ := domainServer(t)
	for _, port := range []string{"7681", "6080"} {
		w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1","port":`+port+`}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("port %s = %d, want 409 (body %q)", port, w.Code, w.Body.String())
		}
		if code := jsonField(t, w, "code"); code != "platform_port" {
			t.Errorf("port %s: code = %q, want platform_port", port, code)
		}
	}
}

// A wrong-org key must not learn that the host exists.
func TestDomainOwnershipIs404Not403(t *testing.T) {
	s, _ := domainServer(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	if w := apiReq(t, s, "GET", "u:two", "/v1/domains", ""); !strings.Contains(w.Body.String(), `"domains":[]`) {
		t.Errorf("other org's listing = %q, want empty", w.Body.String())
	}
	w := apiReq(t, s, "DELETE", "u:two", "/v1/domains/app.example.com", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("other org's delete = %d, want 404 (403 would confirm the host exists)", w.Code)
	}
	// Attaching another org's sandbox is the same 404, not a 403 either.
	if w := apiReq(t, s, "POST", "u:two", "/v1/domains", `{"host":"other.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusNotFound {
		t.Errorf("foreign sandbox = %d, want 404", w.Code)
	}
	// And the owner's own delete works, which proves the 404s above were tenancy.
	if w := apiReq(t, s, "DELETE", "u:one", "/v1/domains/app.example.com", ""); w.Code != http.StatusOK {
		t.Errorf("owner delete = %d, want 200", w.Code)
	}
}

func TestDomainCreateValidatesTheHost(t *testing.T) {
	s, _ := domainServer(t)
	for _, tc := range []struct{ name, host string }{
		{"single label", "localhost"},
		{"scheme", "https://app.example.com"},
		{"empty label", "app..example.com"},
		{"under the platform domain", "grab." + domainTestDomain},
		{"the platform domain itself", domainTestDomain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiReq(t, s, "POST", "u:one", "/v1/domains",
				`{"host":`+quote(tc.host)+`,"sandbox_id":"sb1"}`)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%q = %d, want 400 (body %q)", tc.host, w.Code, w.Body.String())
			}
		})
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// §2.1: an active row routes; a deleted one falls through to the site.
func TestCustomHostRoutesOnlyWhileActive(t *testing.T) {
	s, _ := domainServer(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	// Pending: attached but not verified, so it must not reach the sandbox.
	if w := hostReq(t, s, "app.example.com", "/"); strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatalf("a pending host reached the sandbox: %q", w.Body.String())
	}

	activate(t, s, "app.example.com")
	w := hostReq(t, s, "app.example.com", "/index.html")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/index.html") {
		t.Fatalf("active host = %d %q, want the sandbox's app", w.Code, w.Body.String())
	}
	// No ?token= was needed: the sandbox is public.
	if strings.Contains(w.Body.String(), "token") {
		t.Errorf("body mentions a token: %q", w.Body.String())
	}

	// Positive control: an unattached host is untouched by the stage.
	other := hostReq(t, s, "unknown.example", "/index.html")
	if strings.Contains(other.Body.String(), "FROM-APP") {
		t.Errorf("an unattached host reached a sandbox: %q", other.Body.String())
	}

	if w := apiReq(t, s, "DELETE", "u:one", "/v1/domains/app.example.com", ""); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	gone := hostReq(t, s, "app.example.com", "/index.html")
	if strings.Contains(gone.Body.String(), "FROM-APP") {
		t.Errorf("a removed host still routes: %q", gone.Body.String())
	}
	if gone.Body.String() != other.Body.String() {
		t.Errorf("a removed host answers %q, the unattached control answers %q — they must be the same",
			gone.Body.String(), other.Body.String())
	}
}

// §2.2: nothing of ours embeds a customer domain, so framing is denied there
// while the platform domain keeps its allow-list.
func TestPreviewGuardsDependOnTheServedHost(t *testing.T) {
	s, _ := domainServer(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	activate(t, s, "app.example.com")

	custom := hostReq(t, s, "app.example.com", "/")
	if got := custom.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("custom host CSP = %q, want frame-ancestors 'none'", got)
	}
	if got := custom.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("custom host X-Frame-Options = %q, want DENY", got)
	}

	platform := hostReq(t, s, "8000-sb1."+domainTestDomain, "/")
	if got := platform.Header().Get("Content-Security-Policy"); !strings.Contains(got, "https://*."+domainTestDomain) {
		t.Errorf("platform host CSP = %q, want the dashboard still allowed to embed", got)
	}
	if got := platform.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("platform host X-Frame-Options = %q, want unset", got)
	}
}

// §2.3: a deployment with nowhere to point a CNAME must not advertise the verb.
func TestCapabilitiesReportCustomDomains(t *testing.T) {
	s, _ := domainServer(t)
	if w := apiReq(t, s, "GET", "u:one", "/v1/capabilities", ""); !strings.Contains(w.Body.String(), `"custom_domains":true`) {
		t.Errorf("with a domain: %q", w.Body.String())
	}
	s.sandboxDomain = ""
	if w := apiReq(t, s, "GET", "u:one", "/v1/capabilities", ""); !strings.Contains(w.Body.String(), `"custom_domains":false`) {
		t.Errorf("without a domain: %q", w.Body.String())
	}
}

// §3.1: the TLS terminator's ask. A 200 for a host we did not verify would let
// a stranger burn the certificate authority's rate limit for every customer.
func TestDomainCheckApprovesOnlyVerifiedHosts(t *testing.T) {
	s, _ := domainServer(t)
	for _, body := range []string{
		`{"host":"active.example.com","sandbox_id":"sb1"}`,
		`{"host":"pending.example.com","sandbox_id":"sb1"}`,
		`{"host":"*.vms.example.com"}`,
		`{"host":"*.pending-vms.example.com"}`,
	} {
		if w := apiReq(t, s, "POST", "u:one", "/v1/domains", body); w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d (%s)", body, w.Code, w.Body.String())
		}
	}
	activate(t, s, "active.example.com")
	activate(t, s, "*.vms.example.com")

	for _, tc := range []struct {
		host string
		want int
	}{
		{"active.example.com", 200},
		{"pending.example.com", 404},
		{"never-attached.example.com", 404},
		{"junk.vms.example.com", 404},
		{"8000-sb1.vms.example.com", 200},
		{"8000-sb2.vms.example.com", 404},              // another org's sandbox
		{"vms.example.com", 404},                       // the bare org domain
		{"8000-sb1.pending-vms.example.com", 404},      // wildcard not verified
		{"8000-sb1.vms.example.com.evil.example", 404}, // suffix look-alike
		{"", 404},
	} {
		w := hostReq(t, s, "orchestrator.internal", "/v1/domains/check?host="+tc.host)
		if w.Code != tc.want {
			t.Errorf("check %q = %d, want %d (body %q)", tc.host, w.Code, tc.want, w.Body.String())
		}
	}
}

// §4.1: the wildcard serves every sandbox of the org by label, and nothing else.
func TestWildcardHostServesTheOrgsSandboxesOnly(t *testing.T) {
	s, _ := domainServer(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"*.w.example.com"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	activate(t, s, "*.w.example.com")

	if w := hostReq(t, s, "8000-sb1.w.example.com", "/x"); !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/x") {
		t.Errorf("own sandbox = %d %q", w.Code, w.Body.String())
	}
	if w := hostReq(t, s, "8000-sb2.w.example.com", "/x"); w.Code != http.StatusNotFound {
		t.Errorf("another org's sandbox = %d %q, want 404", w.Code, w.Body.String())
	}
	if w := hostReq(t, s, "w.example.com", "/"); w.Code != http.StatusNotFound {
		t.Errorf("bare org domain = %d %q, want 404", w.Code, w.Body.String())
	}
	if w := hostReq(t, s, "nosuch.w.example.com", "/"); w.Code != http.StatusNotFound {
		t.Errorf("unknown label = %d, want 404", w.Code)
	}
}

// §4.2: the slug form works under a wildcard, since ParseSandboxHost's sibling
// parser already accepts it on the platform domain.
func TestWildcardHostServesTheSlugForm(t *testing.T) {
	s, _ := domainServer(t)
	st := s.st.Load()
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", OrgID: "u:one", Slug: "myapp", Template: "base", State: "running", PublicPort: 3000,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("slug: %v", err)
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"*.w.example.com"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	activate(t, s, "*.w.example.com")

	if w := hostReq(t, s, "myapp.w.example.com", "/"); !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/3000/") {
		t.Errorf("slug host = %d %q, want the sandbox's public port", w.Code, w.Body.String())
	}
}

// The cache must not outlive a removal for longer than its TTL.
func TestCustomRouteCacheExpires(t *testing.T) {
	s, _ := domainServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	// Warm the cache while the row is still pending, then activate it behind the
	// cache's back, the way another replica's verifier would.
	if w := hostReq(t, s, "app.example.com", "/"); strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatal("a pending row routed")
	}
	if err := s.st.Load().SetDomainStatus(t.Context(), "app.example.com", "active", "", 1); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if w := hostReq(t, s, "app.example.com", "/"); strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatal("the cached negative answer was never used")
	}
	now = now.Add(customDomainTTL + time.Second)
	if w := hostReq(t, s, "app.example.com", "/"); !strings.Contains(w.Body.String(), "FROM-APP") {
		t.Errorf("after the TTL the row still did not route: %q", w.Body.String())
	}
}

// The exact path refuses to attach a host to a private sandbox (sandbox_not_public)
// because a host that answers 403 to everyone is useless. A wildcard row names no
// sandbox when it is stored, so nothing ever asked that question for the sandboxes
// it resolves to — and a private one was addressable under the CUSTOMER's hostname,
// where it served OUR refusal page. It answers 404 now, like any other label under
// their wildcard that names no sandbox of theirs.
func TestWildcardHostDoesNotServePrivateSandboxes(t *testing.T) {
	s, _ := domainServer(t)
	st := s.st.Load()
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb3", OrgID: "u:one", Template: "base", State: "running", PublicPort: 8000,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save sb3: %v", err)
	}
	s.reg.Place("sb3", "n1")
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"*.w.example.com"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	activate(t, s, "*.w.example.com")

	w := hostReq(t, s, "8000-sb3.w.example.com", "/")
	if w.Code != http.StatusNotFound {
		t.Errorf("private sandbox under the org wildcard = %d, want 404: %.160q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "This sandbox preview is private") {
		t.Errorf("the platform's refusal page was served on the customer's own hostname: %.160q", w.Body.String())
	}
	// Positive control: the public sandbox in the same org still routes, so the
	// check above is the public column and not a broken wildcard.
	if w := hostReq(t, s, "8000-sb1.w.example.com", "/x"); !strings.Contains(w.Body.String(), "FROM-APP /preview/sb1/8000/x") {
		t.Errorf("public sandbox under the same wildcard = %d %q", w.Code, w.Body.String())
	}
}

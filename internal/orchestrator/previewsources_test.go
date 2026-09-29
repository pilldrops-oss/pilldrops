package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// trustProxies points the X-Forwarded-For trust list at the given CIDRs for one
// test. The production value is parsed once from the environment, so the var is
// the seam rather than a second code path.
func trustProxies(t *testing.T, cidrs ...string) {
	t.Helper()
	prev := httpx.TrustedProxyCIDRs
	t.Cleanup(func() { httpx.TrustedProxyCIDRs = prev })
	var prefixes []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("bad test CIDR %q: %v", c, err)
		}
		prefixes = append(prefixes, p)
	}
	httpx.TrustedProxyCIDRs = func() []netip.Prefix { return prefixes }
}

// previewFrom is an anonymous preview request from one peer address.
func previewFrom(t *testing.T, s *Server, id, remoteAddr, xff string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/preview/"+id+"/8000/", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func TestPreviewSourceAdminRoundTrip(t *testing.T) {
	s, _ := domainServer(t)
	opReq := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fleet-key")
		req.RemoteAddr = "203.0.113.7:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}

	w := opReq("PUT", "/v1/admin/orgs/u:one/preview-sources", `{"cidrs":["10.0.0.0/8","garbage"]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "garbage") {
		t.Fatalf("bad CIDR = %d %q, want 400 naming the entry", w.Code, w.Body.String())
	}
	if got := opReq("GET", "/v1/admin/orgs/u:one/preview-sources", "").Body.String(); !strings.Contains(got, `"cidrs":[]`) {
		t.Errorf("after the refusal the org has %q, want nothing stored", got)
	}
	if w := opReq("PUT", "/v1/admin/orgs/u:one/preview-sources", `{"cidrs":["100.64.0.0/10"]}`); w.Code != http.StatusOK {
		t.Fatalf("valid PUT = %d %q", w.Code, w.Body.String())
	}
	if got := opReq("GET", "/v1/admin/orgs/u:one/preview-sources", "").Body.String(); !strings.Contains(got, "100.64.0.0/10") {
		t.Errorf("GET = %q, want the CIDR back", got)
	}

	// A customer key must not read or set another tenant's reachability.
	req := httptest.NewRequest("GET", "/v1/admin/orgs/u:one/preview-sources", nil)
	req.Header.Set("Authorization", "Bearer "+orgKey(t, s, "u:two"))
	req.RemoteAddr = "203.0.113.7:1234"
	cw := httptest.NewRecorder()
	s.Routes().ServeHTTP(cw, req)
	if cw.Code != http.StatusForbidden {
		t.Errorf("customer key = %d, want 403", cw.Code)
	}
}

// Off-network peers get the nonexistent-sandbox answer, byte for byte: the
// public preview path must not confirm that a restricted org's sandbox exists.
func TestPreviewSourceGate(t *testing.T) {
	s, _ := domainServer(t)
	trustProxies(t, "192.0.2.0/24")
	if err := s.st.Load().SetOrgPreviewSources(t.Context(), "u:one", []string{"100.64.0.0/10"}); err != nil {
		t.Fatalf("set sources: %v", err)
	}

	unknown := previewFrom(t, s, "sb-does-not-exist", "100.64.1.2:9999", "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", unknown.Code)
	}

	off := previewFrom(t, s, "sb1", "203.0.113.5:9999", "")
	if off.Code != unknown.Code || off.Body.String() != unknown.Body.String() {
		t.Errorf("off-network answer (%d %q) differs from the unknown-id answer (%d %q)",
			off.Code, off.Body.String(), unknown.Code, unknown.Body.String())
	}

	on := previewFrom(t, s, "sb1", "100.64.1.2:9999", "")
	if on.Code != http.StatusOK || !strings.Contains(on.Body.String(), "FROM-APP") {
		t.Errorf("on-network = %d %q, want the app", on.Code, on.Body.String())
	}

	// A stranger claiming an allowed address is judged by its real one.
	spoofed := previewFrom(t, s, "sb1", "203.0.113.5:9999", "100.64.1.2")
	if spoofed.Code != http.StatusNotFound {
		t.Errorf("spoofed X-Forwarded-For = %d, want 404", spoofed.Code)
	}
	// The same header from the configured terminator is trusted.
	viaProxy := previewFrom(t, s, "sb1", "192.0.2.10:9999", "100.64.1.2")
	if viaProxy.Code != http.StatusOK {
		t.Errorf("trusted terminator = %d, want 200", viaProxy.Code)
	}
	viaProxyOff := previewFrom(t, s, "sb1", "192.0.2.10:9999", "203.0.113.5")
	if viaProxyOff.Code != http.StatusNotFound {
		t.Errorf("trusted terminator, off-network client = %d, want 404", viaProxyOff.Code)
	}

	// An org that set nothing is unaffected.
	if w := previewFrom(t, s, "sb2", "203.0.113.5:9999", ""); w.Code != http.StatusOK {
		t.Errorf("org with no CIDRs = %d, want 200", w.Code)
	}
}

// The gate applies on a custom host too, not only the platform path.
func TestPreviewSourceGateAppliesOnACustomHost(t *testing.T) {
	s, _ := domainServer(t)
	trustProxies(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	activate(t, s, "app.example.com")
	if w := hostReq(t, s, "app.example.com", "/"); !strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatalf("host does not route before the gate: %q", w.Body.String())
	}
	if err := s.st.Load().SetOrgPreviewSources(t.Context(), "u:one", []string{"100.64.0.0/10"}); err != nil {
		t.Fatalf("set sources: %v", err)
	}
	w := hostReq(t, s, "app.example.com", "/")
	if w.Code != http.StatusNotFound {
		t.Errorf("off-network on a custom host = %d %q, want 404", w.Code, w.Body.String())
	}
}

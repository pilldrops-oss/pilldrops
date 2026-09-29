package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// X-Org-Override goes to the node as a signed, time-bound orgassert token,
// not the bare org string, so these tests decode what the fake node received.

// wantSignedOrg checks the assertion the node received for method+path
// verifies against the server's nodeToken and names wantOrg.
func wantSignedOrg(t *testing.T, s *Server, method, path string, seen map[string]string, wantOrg string) {
	t.Helper()
	label := method + " " + path
	hdr := seen[label]
	t.Helper()
	if hdr == "" {
		t.Errorf("%s: no X-Org-Override reached the node at all", label)
		return
	}
	if hdr == wantOrg {
		t.Errorf("%s: org relayed as the bare string %q — H9 requires a signed assertion", label, wantOrg)
		return
	}
	// "node-a" is the id every fixture in this file registers.
	org, nonce, ok := orgassert.Verify([]byte(s.nodeToken), hdr, "node-a", method, path, time.Now())
	if !ok {
		t.Errorf("%s: header %q did not verify against the orchestrator's own nodeToken for node-a", label, hdr)
		return
	}
	if org != wantOrg {
		t.Errorf("%s: verified org = %q, want %q", label, org, wantOrg)
	}
	if nonce == "" {
		t.Errorf("%s: assertion carries no nonce — a captured header is replayable for its whole TTL", label)
	}
	// The wrong node must reject it, or the binding is decorative.
	if _, _, ok := orgassert.Verify([]byte(s.nodeToken), hdr, "node-b", method, path, time.Now()); ok {
		t.Errorf("%s: assertion minted for node-a also verifies at node-b — it is replayable across the fleet", label)
	}
}

// orgCapturingNode returns a fake node that records X-Org-Override per method+path.
func orgCapturingNode(t *testing.T, seen map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("X-Org-Override")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && r.URL.Path == "/v1/sandboxes" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sb-org-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
}

// Handlers are called directly, so withOperator stands in for auth(); without
// it only "default" could be named via X-Org-Override.
func TestOrgOverrideForwardedToNode(t *testing.T) {
	seen := map[string]string{}
	node := orgCapturingNode(t, seen)
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now())

	// create
	req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"base"}`))
	req.Header.Set("X-Org-Override", "u:cust9")
	req = req.WithContext(withOperator(req.Context()))
	w := httptest.NewRecorder()
	s.handleCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	wantSignedOrg(t, s, "POST", "/v1/sandboxes", seen, "u:cust9")

	// list fan-out
	req = httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.Header.Set("X-Org-Override", "u:cust9")
	req = req.WithContext(withOperator(req.Context()))
	s.handleList(httptest.NewRecorder(), req)
	wantSignedOrg(t, s, "GET", "/v1/sandboxes", seen, "u:cust9")

	// per-sandbox proxy (exec etc.)
	s.reg.Place("sb-org-1", "node-a")
	req = httptest.NewRequest("POST", "/v1/sandboxes/sb-org-1/exec", strings.NewReader(`{}`))
	req = req.WithContext(withOperator(req.Context()))
	req.SetPathValue("id", "sb-org-1")
	req.SetPathValue("rest", "exec")
	req.Header.Set("X-Org-Override", "u:cust9")
	s.handleSandboxProxy(httptest.NewRecorder(), req)
	wantSignedOrg(t, s, "POST", "/v1/sandboxes/sb-org-1/exec", seen, "u:cust9")

	// destroy
	req = httptest.NewRequest("DELETE", "/v1/sandboxes/sb-org-1", nil)
	req = req.WithContext(withOperator(req.Context()))
	req.SetPathValue("id", "sb-org-1")
	req.Header.Set("X-Org-Override", "u:cust9")
	s.handleDestroy(httptest.NewRecorder(), req)
	wantSignedOrg(t, s, "DELETE", "/v1/sandboxes/sb-org-1", seen, "u:cust9")
}

func TestBillingAndUsageProxiedWithOrg(t *testing.T) {
	seen := map[string]string{}
	node := orgCapturingNode(t, seen)
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now())

	for _, path := range []string{"/v1/limits", "/v1/usage"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("X-Org-Override", "u:cust9")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d body=%s", path, w.Code, w.Body.String())
		}
		wantSignedOrg(t, s, "GET", path, seen, "u:cust9")
	}
}

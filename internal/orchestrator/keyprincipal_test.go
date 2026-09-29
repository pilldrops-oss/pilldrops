package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// keyCapturingNode records the org assertion and the key headers per request.
func keyCapturingNode(t *testing.T, org, keyID, keyRole map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := r.Method + " " + r.URL.Path
		org[label] = r.Header.Get("X-Org-Override")
		keyID[label] = r.Header.Get("X-Key-Id")
		keyRole[label] = r.Header.Get("X-Key-Role")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sb-key-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
}

// The calling key reaches the node beside the org, inside the same signature.
// Every node-bound request is signed in one place, so this covers the create,
// the fork and the visibility flip alike.
func TestCallerKeyIsForwardedAndSigned(t *testing.T) {
	orgs, ids, roles := map[string]string{}, map[string]string{}, map[string]string{}
	node := keyCapturingNode(t, orgs, ids, roles)
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now())

	req := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(`{"template":"base"}`))
	req.Header.Set("Authorization", "Bearer k") // the portal's operator credential
	req.Header.Set("X-Org-Override", "u:cust9")
	req.Header.Set("X-Key-Id", "k:alice")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}

	const label = "POST /v1/sandboxes"
	if ids[label] != "k:alice" {
		t.Fatalf("node saw X-Key-Id=%q, want k:alice — the creator did not survive the hop", ids[label])
	}
	hdr := orgs[label]
	org, _, ok := orgassert.Verify([]byte(s.nodeToken), hdr, "node-a", "POST", "/v1/sandboxes", time.Now(), "k:alice", "")
	if !ok || org != "u:cust9" {
		t.Fatalf("assertion bound to (k:alice, \"\") did not verify: org=%q ok=%v", org, ok)
	}
	// The binding is the point: the same header must not verify for another
	// key id, or a node would accept a swapped X-Key-Id as genuine.
	if _, _, ok := orgassert.Verify([]byte(s.nodeToken), hdr, "node-a", "POST", "/v1/sandboxes", time.Now(), "k:mallory", ""); ok {
		t.Error("the assertion verifies for a different key id — the key is not bound, only sent")
	}
	if _, _, ok := orgassert.Verify([]byte(s.nodeToken), hdr, "node-a", "POST", "/v1/sandboxes", time.Now()); ok {
		t.Error("the assertion verifies with no key id at all — a stripped header would pass as unattributed")
	}
}

// Every forwarding entry point must carry the caller, not just the one this
// change happened to wire: a route that drops the key id creates sandboxes
// attributed to nobody, which the ownership rule reads as org-visible.
func TestEveryForwardedRouteCarriesTheCallerKey(t *testing.T) {
	orgs, ids, roles := map[string]string{}, map[string]string{}, map[string]string{}
	node := keyCapturingNode(t, orgs, ids, roles)
	defer node.Close()

	s := NewServer("k", "t")
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, time.Now())
	s.reg.Place("sb-key-1", "node-a")

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/sandboxes", `{"template":"base"}`},
		{"GET", "/v1/sandboxes", ""},
		{"POST", "/v1/sandboxes/sb-key-1/fork", `{}`},
		{"POST", "/v1/sandboxes/sb-key-1/exec", `{"cmd":"true"}`},
		{"POST", "/v1/sandboxes/sb-key-1/visibility", `{"visibility":"org"}`},
		{"GET", "/v1/sandboxes/sb-key-1", ""},
		// Last: a destroy unplaces the sandbox, so anything after it would
		// never reach a node and would pass vacuously.
		{"DELETE", "/v1/sandboxes/sb-key-1", ""},
	} {
		label := tc.method + " " + tc.path
		delete(ids, label)
		delete(orgs, label)
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer k")
		req.Header.Set("X-Org-Override", "u:cust9")
		req.Header.Set("X-Key-Id", "k:alice")
		req.Header.Set("X-Key-Role", "user")
		s.Routes().ServeHTTP(httptest.NewRecorder(), req)
		if _, reached := orgs[label]; !reached {
			t.Errorf("%s never reached the node, so this route proves nothing", label)
			continue
		}
		if ids[label] != "k:alice" || roles[label] != "user" {
			t.Errorf("%s: node saw key=(%q,%q), want (k:alice,user)", label, ids[label], roles[label])
		}
	}
}

// The org assertion and the key headers are set together, in one function, so
// a forwarding path added later cannot assert the org and forget the caller.
// A new site that sets the header itself fails here rather than silently
// creating sandboxes that belong to nobody.
func TestOnlyOnePlaceSetsTheOrgAssertion(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sites := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if n := strings.Count(string(src), "Header.Set(orgOverride"); n > 0 {
			sites[f] = n
		}
	}
	if len(sites) != 1 || sites["forward.go"] != 1 {
		t.Errorf("X-Org-Override is set in %v, want exactly one site in forward.go (setOrgAndKey). "+
			"A path that signs the org itself will not carry X-Key-Id, and what it creates "+
			"will belong to nobody — which the ownership rule reads as visible to the whole org.", sites)
	}
}

// A customer key names itself; it cannot claim to be another member.
func TestACustomerKeyCannotAssertAnotherKeyId(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	r.Header.Set("X-Key-Id", "k:victim")
	// withOrg is what auth() applies for a customer key: no key headers are read.
	id, _ := callerKey(withOrg(r.Context(), "u:cust9"))
	if id != "" {
		t.Fatalf("callerKey = %q for a customer-key request, want empty (the header is not honoured)", id)
	}
	id, role := callerKey(withKey(withOrg(r.Context(), "u:cust9"), "key_real", "user"))
	if id != "key_real" || role != "user" {
		t.Fatalf("callerKey = (%q, %q), want (key_real, user)", id, role)
	}
}

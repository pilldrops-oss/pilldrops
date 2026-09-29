package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/authcache"
	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	sbSelf    = "sb-1-self"
	sbSibling = "sb-2-sibling"
	sbToken   = sbtoken.IdentityPrefix + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// newSandboxPrincipalServer is a control plane with a store holding one
// sandbox whose identity is sbToken, plus an org sibling.
func newSandboxPrincipalServer(t *testing.T) (*Server, *store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, r := range []store.SandboxRecord{
		{ID: sbSelf, OrgID: "org-a", Template: "base", State: "running", IdentityHash: sbtoken.HashHex(sbToken)},
		{ID: sbSibling, OrgID: "org-a", Template: "base", State: "running", IdentityHash: sbtoken.HashHex("pd_sb_other")},
	} {
		if err := st.SaveSandbox(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer("k", "t")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return s, st, srv
}

func doAs(t *testing.T, srv *httptest.Server, tok, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSandboxTokenAuthenticatesAsItsRowAndDiesWithIt(t *testing.T) {
	s, st, srv := newSandboxPrincipalServer(t)

	code, body := doAs(t, srv, sbToken, "GET", "/v1/whoami")
	if code != 200 {
		t.Fatalf("whoami with a valid sandbox token = %d %s", code, body)
	}
	var who map[string]string
	_ = json.Unmarshal([]byte(body), &who)
	if who["principal"] != "sandbox" || who["org"] != "org-a" || who["sandbox_id"] != sbSelf {
		t.Fatalf("whoami = %v", who)
	}
	// The branch never touches the key cache: nothing to go stale on any replica.
	if _, cached := s.authc.Get(authcache.Key(sbToken)); cached {
		t.Fatal("a sandbox token was memoised in authc; destroy could not revoke it before the TTL")
	}
	if code, _ := doAs(t, srv, sbtoken.IdentityPrefix+"nothing-hashes-to-this", "GET", "/v1/whoami"); code != 401 {
		t.Fatalf("unknown pd_sb_ token = %d, want 401", code)
	}
	// Destroy deletes the row; the very next request is refused, no TTL.
	if err := st.DeleteSandbox(t.Context(), sbSelf); err != nil {
		t.Fatal(err)
	}
	if code, _ := doAs(t, srv, sbToken, "GET", "/v1/whoami"); code != 401 {
		t.Fatalf("token of a destroyed sandbox = %d, want 401 immediately", code)
	}
	// Positive control: the operator key still works, so the 401 is the token's.
	if code, _ := doAs(t, srv, "k", "GET", "/v1/whoami"); code != 200 {
		t.Fatalf("operator whoami = %d", code)
	}
}

// routePatterns enumerates every pattern registered on the orchestrator mux
// by reading the registrations themselves, so a route added anywhere in this
// package is exercised below and is denied to a sandbox unless listed.
func routePatterns(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`)
	files, _ := filepath.Glob("*.go")
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if p := m[1]; !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("found only %d mux registrations; the enumeration regex no longer matches how routes are registered", len(out))
	}
	return out
}

// requestFor turns a mux pattern into a concrete request: the method it
// names (GET when it names none) and its path with placeholders filled so
// the sandbox is acting on a SIBLING, the case the allowlist must narrow.
func requestFor(pattern, rest string) (method, path string) {
	method, path = http.MethodGet, pattern
	if m, p, ok := strings.Cut(pattern, " "); ok {
		method, path = m, p
	}
	path = strings.ReplaceAll(path, "{id}", sbSibling)
	if strings.Contains(path, "{rest...}") && rest == "exec" {
		method = http.MethodPost // exec is a POST; the proxy pattern names no method
	}
	path = strings.ReplaceAll(path, "{rest...}", rest)
	path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "x")
	return method, path
}

func TestSandboxPrincipalIsDeniedEveryRouteNotOnTheAllowlist(t *testing.T) {
	_, _, srv := newSandboxPrincipalServer(t)

	// The allowlist, as mux patterns, for a SIBLING sandbox. Anything else
	// registered on the mux must answer 403 to a sandbox principal.
	allowed := map[string]bool{
		"POST /v1/sandboxes":                true,
		"GET /v1/sandboxes":                 true,
		"GET /v1/sandboxes/{id}":            true,
		"POST /v1/sandboxes/{id}/fork":      true,
		"GET /v1/sandboxes/{id}/ports":      true,
		"GET /v1/whoami":                    true,
		"/v1/sandboxes/{id}/{rest...}:exec": true, // the proxy, with rest=exec
	}
	checked := 0
	for _, pattern := range routePatterns(t) {
		if !strings.Contains(pattern, "/v1/") {
			continue
		}
		// A route that answers with no credential at all is public and has no
		// principal to scope; decided by probing, not by a list that would rot.
		if m, p := requestFor(pattern, "pause"); func() bool {
			code, _ := doAs(t, srv, "", m, p)
			return code != 401 && code != 503
		}() {
			continue
		}
		// The proxy pattern is probed twice: an allowed verb and a denied one.
		rests := []string{"pause"}
		if strings.Contains(pattern, "{rest...}") {
			rests = []string{"pause", "exec"}
		}
		for _, rest := range rests {
			method, path := requestFor(pattern, rest)
			code, body := doAs(t, srv, sbToken, method, path)
			checked++
			want := allowed[pattern] || allowed[pattern+":"+rest]
			switch {
			case want && (code == 403 || code == 401):
				t.Errorf("%s (%s %s): allowed route refused with %d %s", pattern, method, path, code, body)
			case !want && code != 403:
				t.Errorf("%s (%s %s): a sandbox principal got %d, want 403 (default deny)", pattern, method, path, code)
			case !want && !strings.Contains(body, "principal is a sandbox"):
				t.Errorf("%s: 403 body must name the principal kind: %s", pattern, body)
			}
		}
	}
	if checked < 40 {
		t.Fatalf("only %d routes probed", checked)
	}

	// On ITSELF every verb passes the allowlist (the node still owns the outcome).
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/v1/sandboxes/" + sbSelf},
		{"POST", "/v1/sandboxes/" + sbSelf + "/pause"},
		{"PATCH", "/v1/sandboxes/" + sbSelf},
		{"GET", "/v1/sandboxes/" + sbSelf + "/metrics"},
	} {
		if code, body := doAs(t, srv, sbToken, c.method, c.path); code == 403 || code == 401 {
			t.Errorf("%s %s on itself = %d %s", c.method, c.path, code, body)
		}
	}
	// Named in the spec: siblings may be exec'd, copied to and forked, never destroyed or paused.
	for _, c := range []struct {
		method, path string
		want403      bool
	}{
		{"POST", "/v1/sandboxes/" + sbSibling + "/exec", false},
		{"GET", "/v1/sandboxes/" + sbSibling + "/files?path=/x", false},
		{"POST", "/v1/sandboxes/" + sbSibling + "/files", false},
		{"DELETE", "/v1/sandboxes/" + sbSibling, true},
		{"POST", "/v1/sandboxes/" + sbSibling + "/pause", true},
		{"POST", "/v1/sandboxes/" + sbSibling + "/timeout", true},
		{"GET", "/v1/keys", true},
		{"GET", "/v1/vault/secrets", true},
		{"GET", "/v1/billing/balance", true},
		{"GET", "/v1/projects", true},
		{"GET", "/v1/images", true},
		{"GET", "/v1/admin/users", true},
	} {
		code, _ := doAs(t, srv, sbToken, c.method, c.path)
		if got := code == 403; got != c.want403 {
			t.Errorf("%s %s = %d, want403=%v", c.method, c.path, code, c.want403)
		}
	}
}

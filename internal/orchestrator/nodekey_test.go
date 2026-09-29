package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/adminauth"
)

// A node must not be able to derive another node's key from the fleet-wide ORCHESTRATOR_TOKEN.
func TestNodeKeysDeriveFromAControlPlaneOnlySecret(t *testing.T) {
	// What a node holds.
	const fleetToken = "orchestrator-token-every-node-has"

	withSecret := NewServer("k", fleetToken)
	withSecret.nodeKeySecret = "control-plane-only-secret"

	legacy := NewServer("k", fleetToken) // nodeKeySecret unset

	victim := legacy.nodeKeyFor("node-victim", "par1")

	// A compromised node knows fleetToken and can reproduce the legacy derivation.
	attacker := NewServer("k", fleetToken)
	if got := attacker.nodeKeyFor("node-victim", "par1"); got != victim {
		t.Fatal("the legacy derivation is not reproducible from the fleet token — " +
			"this test no longer describes the vulnerability it guards")
	}

	real := withSecret.nodeKeyFor("node-victim", "par1")
	if attacker.nodeKeyFor("node-victim", "par1") == real {
		t.Error("a holder of ORCHESTRATOR_TOKEN still derives another node's key")
	}
	if real == victim {
		t.Error("NODE_KEY_SECRET did not change the derivation at all")
	}
}

// Keys must stay stable across restarts or every orchestrator-to-node call 401s.
func TestNodeKeyIsStableAndPerNode(t *testing.T) {
	s := NewServer("k", "t")
	s.nodeKeySecret = "control-plane-only-secret"

	a1 := s.nodeKeyFor("node-a", "par1")
	if a1 == "" {
		t.Fatal("empty key")
	}
	if a2 := s.nodeKeyFor("node-a", "par1"); a1 != a2 {
		t.Errorf("derivation is not stable: %q then %q", a1, a2)
	}
	if b := s.nodeKeyFor("node-b", "par1"); b == a1 {
		t.Error("two nodes share a key")
	}
	if z := s.nodeKeyFor("node-a", "ams1"); z == a1 {
		t.Error("zone is not part of the derivation")
	}
	// An empty zone must normalise or a node joining without one gets an unrecomputable key.
	if s.nodeKeyFor("node-a", "") != s.nodeKeyFor("node-a", "default") {
		t.Error(`zone "" does not normalise to "default"`)
	}
}

// A console session must authenticate the dashboard's API calls.
func TestAConsoleSessionAuthenticatesTheAPI(t *testing.T) {
	s := NewServer("the-api-key", "nodetoken")

	get := func(cookie string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	valid := adminauth.MintSession([]byte("the-api-key"), "admin@example.com", time.Now())
	if code := get(valid); code != http.StatusOK {
		t.Errorf("a valid console session = %d, want 200 — the dashboard cannot load its own data", code)
	}
	for name, c := range map[string]string{
		"no cookie":             "",
		"garbage":               "not-a-session",
		"signed by another key": adminauth.MintSession([]byte("different-key"), "admin@example.com", time.Now()),
		"expired":               adminauth.MintSession([]byte("the-api-key"), "admin@example.com", time.Now().Add(-2*adminauth.SessionTTL)),
	} {
		if code := get(c); code == http.StatusOK {
			t.Errorf("%s was accepted as an operator session", name)
		}
	}
}

// An empty Authorization header must not defeat the cookie. Go trims header
// whitespace, so "Bearer " arrives as "Bearer" and a naive TrimPrefix left a
// non-empty token that blocked the cookie fallback.
func TestAnEmptyBearerHeaderFallsBackToTheCookie(t *testing.T) {
	s := NewServer("the-api-key", "nodetoken")
	session := adminauth.MintSession([]byte("the-api-key"), "admin@example.com", time.Now())

	get := func(authHeader string, withCookie bool) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		if withCookie {
			req.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: session})
		}
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	// Every shape an empty token can take on the wire.
	for _, hdr := range []string{"Bearer ", "Bearer", "  Bearer  ", "bearer "} {
		if code := get(hdr, true); code != http.StatusOK {
			t.Errorf("Authorization:%q with a valid session = %d, want 200", hdr, code)
		}
	}
	if code := get("Bearer the-api-key", false); code != http.StatusOK {
		t.Errorf("a real key = %d, want 200", code)
	}
	for _, bad := range []string{"Bearer wrong-key", "Basic hunter2", "the-api-key"} {
		if code := get(bad, false); code == http.StatusOK {
			t.Errorf("Authorization:%q was accepted", bad)
		}
	}
}

func TestBearerParsing(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"  Bearer  ", ""},
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},    // case-insensitive scheme, per RFC 7235
		{"Bearer  abc  ", "abc"}, // surrounding space is not part of the token
		{"Basic abc", ""},        // a different scheme carries no bearer token
		{"abc", ""},              // no scheme at all
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		if got := bearer(req); got != tc.want {
			t.Errorf("bearer(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

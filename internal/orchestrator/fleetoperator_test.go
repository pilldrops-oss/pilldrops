package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChangingTheFleetNeedsTheOperatorKey pins the gate on the four routes that
// change the fleet rather than one org's resources.
//
// These were registered behind s.auth alone, which authenticates a caller
// without asking who they are, so any tenant key could cordon a node, drain it,
// remove it, or mint a join token that admits a machine of the caller's
// choosing to the fleet. Draining takes a machine out of service for every
// tenant on it; a join token hands the new machine other tenants' sandboxes.
//
// The positive half matters as much as the refusal: an operator must still be
// able to do all four, or this would "pass" by breaking fleet management.
func TestChangingTheFleetNeedsTheOperatorKey(t *testing.T) {
	s, _ := domainServer(t)
	tenant := orgKey(t, s, "u:tenant")

	cases := []struct{ method, path string }{
		{"POST", "/v1/nodes/n1/cordon"},
		{"POST", "/v1/nodes/n1/uncordon"},
		{"POST", "/v1/nodes/n1/drain"},
		{"DELETE", "/v1/nodes/n1"},
		{"POST", "/v1/join-tokens"},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+tenant)
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s with a tenant key = %d, want 403 (body %q)",
				c.method, c.path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}

	// The operator is not locked out. A node that does not exist answers 404,
	// which is the gate letting the request through to the handler; a 403 here
	// would mean the guard rejects everyone.
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer fleet-key")
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("%s %s with the operator key = 403; the guard refuses everyone", c.method, c.path)
		}
	}
}

// TestFleetReadsNeedTheOperatorKey pins the same gate on the routes that read
// the whole fleet: node addresses and capacity, every org's users and
// sessions, and the node admin routes the catch-all would otherwise forward
// under the control plane's operator key.
func TestFleetReadsNeedTheOperatorKey(t *testing.T) {
	s, _ := domainServer(t)
	tenant := orgKey(t, s, "u:tenant")

	paths := []string{
		"/metrics",
		"/v1/nodes",
		"/v1/cluster/stats",
		"/v1/admin/whoami",
		"/v1/admin/overview",
		"/v1/admin/users",
		"/v1/admin/sessions",
		"/v1/admin/feedback",
		"/v1/admin/funnel",
		"/v1/images/export",
		"/v1/host",
	}
	get := func(path, key string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}
	for _, p := range paths {
		if code := get(p, tenant); code != http.StatusForbidden {
			t.Errorf("GET %s with a tenant key = %d, want 403", p, code)
		}
		if code := get(p, "fleet-key"); code != http.StatusOK {
			t.Errorf("GET %s with the operator key = %d, want 200", p, code)
		}
	}
}

// TestCurationNeedsTheOperatorKey pins the gate on PATCH /v1/images/{id}: a
// tenant must not unpublish or rename an image every other org launches from.
func TestCurationNeedsTheOperatorKey(t *testing.T) {
	s, _ := domainServer(t)
	tenant := orgKey(t, s, "u:tenant")
	patch := func(key string) int {
		req := httptest.NewRequest("PATCH", "/v1/images/python-3.11", strings.NewReader(`{"published":false}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}
	if code := patch(tenant); code != http.StatusForbidden {
		t.Errorf("PATCH /v1/images/{id} with a tenant key = %d, want 403", code)
	}
	if code := patch("fleet-key"); code == http.StatusForbidden {
		t.Errorf("PATCH /v1/images/{id} with the operator key = 403; the guard refuses everyone")
	}
}

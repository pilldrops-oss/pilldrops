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

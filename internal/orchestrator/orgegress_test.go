package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The store has held an org base policy since the column landed and nothing
// could write it, so the base third of the three-way resolve was unreachable.
// The round trip is the point: what the route stores is what the create path
// reads back through store.OrgEgress.
func TestOrgEgressAdminRoundTrip(t *testing.T) {
	s, _ := domainServer(t)
	opReq := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fleet-key")
		req.RemoteAddr = "203.0.113.7:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}

	if got := opReq("GET", "/v1/admin/orgs/u:one/egress", "").Body.String(); !strings.Contains(got, `"set":false`) {
		t.Errorf("an org with no override reads %q, want set:false", got)
	}
	if w := opReq("PUT", "/v1/admin/orgs/u:one/egress", `{"network":{"allow":["10.0.0.0/8"]}}`); w.Code != http.StatusBadRequest {
		t.Errorf("a private range in allow = %d %q, want 400: the allow list is public-only", w.Code, w.Body.String())
	}
	if w := opReq("PUT", "/v1/admin/orgs/u:one/egress", `{"network":{"allow":["api.stripe.com"]}}`); w.Code != http.StatusNotImplemented {
		t.Errorf("a domain rule = %d %q, want 501: stored, it would fail every later create in the org", w.Code, w.Body.String())
	}

	if w := opReq("PUT", "/v1/admin/orgs/u:one/egress", `{"network":{"allow":["203.0.113.7","198.51.100.0/24"]}}`); w.Code != http.StatusOK {
		t.Fatalf("valid PUT = %d %q", w.Code, w.Body.String())
	}
	// Read it back the way the create path does, not only the way the route does.
	rules, ok, err := s.st.Load().OrgEgress(t.Context(), "u:one")
	if err != nil || !ok {
		t.Fatalf("OrgEgress after the PUT = %v ok=%v, want the stored policy — the create path reads this, not the route", err, ok)
	}
	if len(rules.Allow) != 2 || rules.Allow[0] != "203.0.113.7" {
		t.Errorf("stored allow = %v, want both entries", rules.Allow)
	}

	// A deny-only base is legal and means no egress at all; the operator must be told.
	w := opReq("PUT", "/v1/admin/orgs/u:one/egress", `{"network":{"deny":["203.0.113.0/24"]}}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "denies all egress") {
		t.Errorf("deny-only PUT = %d %q, want 200 carrying the advisory", w.Code, w.Body.String())
	}

	// null clears it, so an org can be put back on template defaults.
	if w := opReq("PUT", "/v1/admin/orgs/u:one/egress", `{"network":null}`); w.Code != http.StatusOK {
		t.Fatalf("clearing PUT = %d %q", w.Code, w.Body.String())
	}
	if _, ok, _ := s.st.Load().OrgEgress(t.Context(), "u:one"); ok {
		t.Errorf("the override survived a null PUT; an org can never be put back on template defaults")
	}

	// A customer key must not read or set a containment control.
	req := httptest.NewRequest("GET", "/v1/admin/orgs/u:one/egress", nil)
	req.Header.Set("Authorization", "Bearer "+orgKey(t, s, "u:two"))
	req.RemoteAddr = "203.0.113.7:1234"
	cw := httptest.NewRecorder()
	s.Routes().ServeHTTP(cw, req)
	if cw.Code != http.StatusForbidden {
		t.Errorf("customer key = %d, want 403", cw.Code)
	}
}

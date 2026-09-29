package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUngatedMountResolvesNothing(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	h := Handler(res)

	req := httptest.NewRequest("GET", "/preview/sb-1783-abcd/8000/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if res.gotID != "" {
		t.Errorf("PreviewTarget was called with org %q from a request that passed no gate", res.gotOrg)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("ungated request: status = %d, want 404", rec.Code)
	}

	// Control: with an org attached the same request reaches the resolver.
	res.gotID = ""
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(WithPreviewOrg(req.Context(), "u:cust1")))
	if res.gotID != "sb-1783-abcd" || res.gotOrg != "u:cust1" {
		t.Fatalf("control: a gated request did not reach the resolver either "+
			"(id=%q org=%q); the assertion above is vacuous", res.gotID, res.gotOrg)
	}
}

func TestPreviewOrgCannotComeFromTheClient(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	req := httptest.NewRequest("GET", "/preview/sb-1783-abcd/8000/", nil)
	for _, h := range []string{"X-Preview-Org", "X-Org-Override", "X-Org-Id", "Preview-Org"} {
		req.Header.Set(h, "u:attacker")
	}
	rec := httptest.NewRecorder()
	Handler(res).ServeHTTP(rec, req)

	if res.gotOrg != "" {
		t.Fatalf("a client header became the authorized org (%q)", res.gotOrg)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// A "default" fallback would open every sandbox created without an explicit org.
func TestPreviewOrgFromEmptyContext(t *testing.T) {
	if got := previewOrgFrom(context.Background()); got != "" {
		t.Fatalf("previewOrgFrom(empty) = %q, want \"\"", got)
	}
	if got := previewOrgFrom(WithPreviewOrg(context.Background(), "u:cust1")); got != "u:cust1" {
		t.Fatalf("previewOrgFrom(set) = %q, want u:cust1", got)
	}
}

func TestHostRouterRefusesAnAuthorizedRequestWithNoOrg(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	okButNoOrg := func(string, int, string) (string, bool) { return "", true }
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not fall through to next")
	}), okButNoOrg)

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if res.gotID != "" {
		t.Fatal("PreviewTarget was called for a request with no org")
	}
}

func TestHostRouterBindsTheVerifiedOrg(t *testing.T) {
	res := &stubResolver{target: "127.0.0.1:1"}
	h := HostRouter("sb.pilldrops.com", res, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not fall through to next")
	}), func(string, int, string) (string, bool) { return "u:cust1", true })

	req := httptest.NewRequest("GET", "http://x/", nil)
	req.Host = "8000-sb-1783-abcd.sb.pilldrops.com"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if res.gotOrg != "u:cust1" {
		t.Fatalf("resolver got org %q, want the org the verifier returned", res.gotOrg)
	}
}

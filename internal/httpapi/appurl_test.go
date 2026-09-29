package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
)

func TestAppURL_PreviewFormWhenNoDomain(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "http://localhost:8080/v1/sandboxes", nil)
	got := s.appURL(r, "sb-1783-abcd", 8000, "default")
	want := "http://localhost:8080/preview/sb-1783-abcd/8000/"
	if got != want {
		t.Fatalf("appURL = %q, want %q", got, want)
	}
}

// Behind a TLS-terminating proxy the subdomain form is https with no port.
func TestAppURL_SubdomainFormBehindTLSProxy(t *testing.T) {
	s := &Server{sandboxDomain: "sb.pilldrops.com"}
	r := httptest.NewRequest("GET", "http://localhost:8080/v1/sandboxes", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	got := s.appURL(r, "sb-1783-abcd", 8000, "default")
	want := "https://8000-sb-1783-abcd.sb.pilldrops.com/"
	if got != want {
		t.Fatalf("appURL = %q, want %q", got, want)
	}
}

// On a bare host the subdomain form must keep the request's scheme and port.
func TestAppURL_SubdomainFormPlainHTTP(t *testing.T) {
	s := &Server{sandboxDomain: "203.0.113.42.nip.io"}
	r := httptest.NewRequest("GET", "http://203.0.113.42:18080/v1/sandboxes", nil)
	got := s.appURL(r, "sb-1783-abcd", 8080, "default")
	want := "http://8080-sb-1783-abcd.203.0.113.42.nip.io:18080/"
	if got != want {
		t.Fatalf("appURL = %q, want %q", got, want)
	}
}

// A standalone node has no orchestrator to mint the preview token, so appURL must.
func TestAppURL_PreviewFormEmbedsTokenWhenSecretConfigured(t *testing.T) {
	s := &Server{previewSecret: []byte("test-preview-secret")}
	r := httptest.NewRequest("GET", "http://localhost:8080/v1/sandboxes", nil)
	got := s.appURL(r, "sb-1783-abcd", 8000, "u:cust1")
	want := "http://localhost:8080/preview/sb-1783-abcd/8000/?ptok=" +
		s.previewToken("sb-1783-abcd", 8000, "u:cust1")
	if got != want {
		t.Fatalf("appURL = %q, want %q", got, want)
	}
	if !previewtoken.Valid(s.previewSecret, s.previewToken("sb-1783-abcd", 8000, "u:cust1"), "sb-1783-abcd", 8000, "u:cust1") {
		t.Fatal("embedded token does not validate against the org it was minted for")
	}
}

// The subdomain form bypasses the orchestrator, so the node's appURL is its only minter.
func TestAppURL_SubdomainFormEmbedsTokenWhenSecretConfigured(t *testing.T) {
	s := &Server{sandboxDomain: "sb.pilldrops.com", previewSecret: []byte("test-preview-secret")}
	r := httptest.NewRequest("GET", "http://localhost:8080/v1/sandboxes", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	got := s.appURL(r, "sb-1783-abcd", 8000, "u:cust1")
	want := "https://8000-sb-1783-abcd.sb.pilldrops.com/?ptok=" +
		s.previewToken("sb-1783-abcd", 8000, "u:cust1")
	if got != want {
		t.Fatalf("appURL = %q, want %q", got, want)
	}
	if !previewtoken.Valid(s.previewSecret, s.previewToken("sb-1783-abcd", 8000, "u:cust1"), "sb-1783-abcd", 8000, "u:cust1") {
		t.Fatal("embedded subdomain-form token does not validate")
	}
}

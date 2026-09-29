package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestRequestIDHeader(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gen := resp.Header.Get("X-Request-ID")
	resp.Body.Close()
	if gen == "" {
		t.Fatal("no X-Request-ID generated")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/health", nil)
	req.Header.Set("X-Request-ID", "client-abc-123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := resp.Header.Get("X-Request-ID")
	resp.Body.Close()
	if got != "client-abc-123" {
		t.Errorf("X-Request-ID = %q, want client-abc-123", got)
	}

	// A CRLF value cannot be sent (the client rejects it), so length and unsafe chars are the reachable cases.
	for _, bad := range []string{strings.Repeat("x", 300), "has space", "a/b?c"} {
		req, _ := http.NewRequest("GET", srv.URL+"/health", nil)
		req.Header.Set("X-Request-ID", bad)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got := resp.Header.Get("X-Request-ID")
		resp.Body.Close()
		if got == "" || got == bad || len(got) > 128 || !obsSafeID(got) {
			t.Errorf("unsafe X-Request-ID %q not sanitized, got %q", bad, got)
		}
	}
}

// obsSafeID mirrors internal/obs's validation without exporting it.
func obsSafeID(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

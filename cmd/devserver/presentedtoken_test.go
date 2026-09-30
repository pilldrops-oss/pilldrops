package main

import (
	"net/http/httptest"
	"testing"
)

// An empty "Authorization: Bearer " reaches the handler as "Bearer" (net/http
// trims the space). Read as a key, it failed auth on the anonymous dev server
// and sent the dashboard round a login redirect forever.
func TestPresentedTokenReadsAnEmptyBearerAsNoToken(t *testing.T) {
	for header, want := range map[string]string{
		"":              "",
		"Bearer":        "",
		"Bearer ":       "",
		"Bearer sf_abc": "sf_abc",
		"sf_raw":        "sf_raw",
	} {
		r := httptest.NewRequest("GET", "/v1/sandboxes", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := presentedToken(r); got != want {
			t.Errorf("Authorization %q -> %q, want %q", header, got, want)
		}
	}
}

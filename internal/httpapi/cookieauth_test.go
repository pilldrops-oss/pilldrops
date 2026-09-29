package httpapi

import (
	"net/http"
	"testing"
)

// The dashboard SPA authenticates with the session cookie, not a Bearer header.
func TestV1AcceptsSessionCookie(t *testing.T) {
	srv, key := newTestServer(t)

	do := func(mut func(*http.Request)) int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/sandboxes", nil)
		if mut != nil {
			mut(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(nil); code != 401 {
		t.Fatalf("no creds = %d, want 401", code)
	}
	if code := do(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: key})
	}); code != 200 {
		t.Fatalf("valid session cookie = %d, want 200", code)
	}
	if code := do(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "sandboxd_session", Value: "bogus"})
	}); code != 401 {
		t.Fatalf("invalid session cookie = %d, want 401", code)
	}
	if code := do(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
	}); code != 200 {
		t.Fatalf("valid bearer = %d, want 200", code)
	}
}

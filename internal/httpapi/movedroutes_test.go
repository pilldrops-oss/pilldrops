package httpapi

import (
	"net/http"
	"testing"
)

// Functions moved to the cloud product; an engine node must not answer for them.
func TestMovedFeatureRoutesAreNotServed(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/functions"},
		{"POST", "/v1/functions"},
		{"GET", "/v1/functions/hello"},
		{"POST", "/v1/functions/hello/invoke"},
		{"GET", "/v1/functions/hello/invoke/api/x"},
		{"GET", "/v1/fn/hello"},
	} {
		if code, _ := do(t, req.method, srv.URL+req.path, key, ""); code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.method, req.path, code)
		}
	}
}

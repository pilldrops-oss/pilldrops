package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A node-API resource left off the prefix list 404s at the portal even with a
// healthy fleet behind it.
func TestFleetPathsCoverEveryProxiedResource(t *testing.T) {
	forwarded := []string{
		"/v1/sandboxes",
		"/v1/sandboxes/sb_123/exec",
	}
	for _, p := range forwarded {
		if !isFleetPath(p) {
			t.Errorf("%s is not forwarded to the fleet — it would 404 at the portal", p)
		}
	}

	// Portal-owned paths must not be forwarded; a node has no idea who the
	// browser user is.
	local := []string{"/v1/limits", "/v1/keys", "/dashboard", "/pricing", "/v1/plans", "/health"}
	for _, p := range local {
		if isFleetPath(p) {
			t.Errorf("%s was sent to the fleet, but the portal owns it", p)
		}
	}
}

// Each of these is called by the dashboard or a customer's key and answered by the
// control plane; without a route the portal 404s. preview-token broke the terminal.
func TestFleetPassthroughServesWhatTheDashboardCalls(t *testing.T) {
	r := chi.NewRouter()
	registerFleetPassthrough(r, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/sandboxes/sb1/preview-token"},
		{"GET", "/v1/capabilities"},
		{"GET", "/v1/limits"},
		{"GET", "/v1/whoami"},
		{"POST", "/v1/sandboxes/sb1/checkpoints"},
		{"POST", "/v1/sandboxes/sb1/snapshot"},
		{"POST", "/v1/sandboxes/sb1/standby"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusTeapot {
			t.Errorf("%s %s = %d, want it passed to the control plane", c.method, c.path, w.Code)
		}
	}
}

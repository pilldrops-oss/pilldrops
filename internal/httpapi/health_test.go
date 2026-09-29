package httpapi

import (
	"net/http"
	"testing"
)

// Liveness must not depend on the database: the binary upgrade script rolls
// back on a non-200 /health, so a DB blip would condemn a good binary.

func TestLivenessSurvivesADeadDatabase(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	st.Close()

	for _, path := range []string{"/health", "/livez"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with the database down = %d, want 200. "+
				"A non-200 here makes sandboxd-update.sh roll back a good binary.",
				path, resp.StatusCode)
		}
	}
}

func TestReadinessFailsOnADeadDatabase(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)

	// Check healthy first so the 503 below is attributable to the store.
	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz on a healthy node = %d, want 200", resp.StatusCode)
	}

	st.Close()

	resp, err = srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with the database down = %d, want 503 — "+
			"this is the probe a load balancer drains on", resp.StatusCode)
	}
}

func TestHealthProbesAreUnauthenticated(t *testing.T) {
	srv, _, _ := newTestServerWithStore(t)
	for _, path := range []string{"/health", "/livez", "/readyz"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Errorf("GET %s = %d, want it open to an unauthenticated prober", path, resp.StatusCode)
		}
	}
}

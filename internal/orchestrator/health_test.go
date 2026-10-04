package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func probe(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// No store is a supported configuration and must still enter rotation.
func TestReadinessWithoutAStoreIsReady(t *testing.T) {
	s := NewServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	if code, body := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("GET /readyz with no store = %d (%s), want 200 — "+
			"running store-less is supported, not a failure", code, body)
	}
}

func TestReadinessFollowsTheStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := NewServer("apikey", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	if code, _ := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("GET /readyz with a live store = %d, want 200", code)
	}

	st.Close() // Ping now fails

	if code, body := probe(t, srv, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with the store down = %d (%s), want 503", code, body)
	}
}

// A dependency-checking liveness probe restarts a healthy process during a store outage.
func TestLivenessIgnoresTheStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := NewServer("apikey", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	st.Close()

	for _, path := range []string{"/health", "/livez"} {
		code, body := probe(t, srv, path)
		if code != http.StatusOK {
			t.Errorf("GET %s with the store down = %d, want 200", path, code)
		}
		// Runbooks and platform probes match on the literal "ok".
		if strings.TrimSpace(body) != "ok" {
			t.Errorf("GET %s body = %q, want %q", path, body, "ok")
		}
	}
}

// The Docs page links /openapi.json; on a fleet that page is served from here.
func TestOrchestratorServesTheOpenAPISpec(t *testing.T) {
	srv := httptest.NewServer(NewServer("apikey", "nodetoken").Routes())
	defer srv.Close()
	code, body := probe(t, srv, "/openapi.json")
	if code != http.StatusOK || !strings.Contains(body, `"openapi"`) {
		t.Fatalf("GET /openapi.json = %d, want 200 and the spec (%.80s)", code, body)
	}
}

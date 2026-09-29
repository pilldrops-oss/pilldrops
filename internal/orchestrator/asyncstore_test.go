package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// cmd/orchestrator opens the store in a background goroutine, so Routes() runs
// before SetStore. Every other test in this package calls SetStore first.

// serverInBootOrder mimics main(): Routes() first, store afterwards.
func serverInBootOrder(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	s := NewServer("fleet-key", "node-token")
	handler := s.Routes()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "async.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "acme", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	s.SetStore(st)
	return handler, st, key
}

func TestStoreBackedRoutesExistWhenTheStoreArrivesLate(t *testing.T) {
	handler, _, key := serverInBootOrder(t)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/projects", ""},
		{http.MethodGet, "/v1/threads", ""},
		{http.MethodGet, "/v1/secrets", ""},
		{http.MethodGet, "/v1/launch-stats?templates=base", ""},
		{http.MethodPost, "/v1/ai/configure", `{"prompt":"x"}`},
		{http.MethodPost, "/v1/ai/revise-plan", `{"message":"x","services":[{"template":"base"}]}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var r *http.Request
			if tc.body == "" {
				r = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				r = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/json")
			}
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			// Any status but 404 proves the handler is reachable.
			if w.Code == http.StatusNotFound {
				t.Errorf("%s %s = 404: the route was not mounted, because the mux was built "+
					"before the store arrived. This is what production does on every boot.",
					tc.method, tc.path)
			}
		})
	}
}

// Before the store lands the routes must answer 503, not 404.
func TestStoreBackedRoutesDegradeRatherThanVanish(t *testing.T) {
	s := NewServer("fleet-key", "node-token")
	handler := s.Routes()

	r := httptest.NewRequest(http.MethodGet, "/v1/threads", nil)
	r.Header.Set("Authorization", "Bearer fleet-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code == http.StatusNotFound {
		t.Fatal("/v1/threads = 404 with no store — it should exist and report 503")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 'store not ready' (body %q)", w.Code, w.Body.String())
	}
}

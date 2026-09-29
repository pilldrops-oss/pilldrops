package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
)

// mountDashboardAPI mirrors the production wiring: /dashboard/api routes sit in
// a group guarded by newAPIAuth.
func mountDashboardAPI(t *testing.T, requireAuth bool) (*chi.Mux, *APIKeyStore) {
	t.Helper()
	ks := LoadAPIKeys(filepath.Join(t.TempDir(), "api_keys.json"))
	auth := newAPIAuth(ks, requireAuth)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth)
		r.Get("/dashboard/api/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("log-stream"))
		})
	})
	return r, ks
}

func TestDashboardAPIRejectsAnonymousWhenRequired(t *testing.T) {
	r, _ := mountDashboardAPI(t, true)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/dashboard/api/logs", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /dashboard/api/logs = %d, want 401 (the log stream must not be public)", rr.Code)
	}
}

func TestDashboardAPIAllowsValidKey(t *testing.T) {
	r, ks := mountDashboardAPI(t, true)
	k, err := ks.Create("dash", "u:alice")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/api/logs", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated /dashboard/api/logs = %d, want 200", rr.Code)
	}
}

// Local dev (PORTAL_API_KEY unset) still allows anonymous access.
func TestDashboardAPILocalDevAllowsAnonymous(t *testing.T) {
	r, _ := mountDashboardAPI(t, false)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/dashboard/api/logs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("local-dev anonymous /dashboard/api/logs = %d, want 200", rr.Code)
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The host stub counts every tenant's sandboxes, so a tenant key is refused.
func TestHostStubAnswersOnlyThePortalKey(t *testing.T) {
	h := hostStub(&DevManager{sandboxes: map[string]*DevSandbox{}}, "sf_portal")
	for key, want := range map[string]int{"sf_portal": http.StatusOK, "sf_tenant": http.StatusForbidden, "": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodGet, "/v1/host", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != want {
			t.Errorf("key %q: %d, want %d", key, w.Code, want)
		}
	}
}

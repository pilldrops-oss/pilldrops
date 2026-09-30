package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// registeredRoutes only scans source, so a registrar never called from Register
// passes every other test here. This one resolves each declared pattern on the
// real mux; comparing the matched pattern also catches shadowing.
func TestRegisterActuallyMountsEveryRoute(t *testing.T) {
	mux := newRouteMux(t)
	declared := registeredRoutes(t)
	if len(declared) == 0 {
		t.Fatal("no routes were declared at all — the extraction regex has drifted and " +
			"this guard is passing on an empty set")
	}
	for _, want := range append(declared, rawPatterns...) {
		method, path := "GET", want
		if m, p, ok := strings.Cut(want, " "); ok {
			method, path = m, p
		}
		req := httptest.NewRequest(method, concretize(path), nil)
		_, got := mux.Handler(req)
		if got != want {
			if got == "" {
				t.Errorf("%q is declared in source but NOT MOUNTED: %s %s resolves to nothing.\n"+
					"Its registrar is almost certainly never called from Register — which every "+
					"source-scanning test in this file is blind to, because the route is still "+
					"written down.", want, method, concretize(path))
				continue
			}
			t.Errorf("%q is declared in source but %s %s resolves to %q instead — it is "+
				"shadowed, not served", want, method, concretize(path), got)
		}
	}
}

// The agent, chat and integration APIs are not part of the engine.
func TestProductRoutesAreNotMounted(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)
	for _, p := range []string{
		"/v1/sandboxes/" + id + "/agent/status", "/v1/sandboxes/" + id + "/agent/turns",
		"/v1/sandboxes/" + id + "/chat", "/v1/sandboxes/" + id + "/integrations",
		"/v1/sandboxes/" + id + "/channels", "/agent/claim",
	} {
		if code, _ := do(t, "GET", srv.URL+p, key, ""); code != 404 && code != 405 {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
}

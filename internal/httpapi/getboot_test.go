package httpapi

import "testing"

// The detail page polls GET, so boot info that only create reported vanished
// the moment anyone reloaded it.
func TestGetReportsHowTheSandboxBooted(t *testing.T) {
	srv, key := newTestServer(t)
	code, created := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	id, _ := created["id"].(string)
	code, got := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, key, "")
	if code != 200 {
		t.Fatalf("get = %d %v", code, got)
	}
	for _, k := range []string{"boot_type", "boot_ms"} {
		if _, ok := got[k]; !ok {
			t.Errorf("get: %s missing from %v", k, got)
		}
		if got[k] != created[k] {
			t.Errorf("get: %s = %v, create said %v", k, got[k], created[k])
		}
	}
}

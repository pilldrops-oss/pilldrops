package httpapi

import "testing"

func TestErrorResponsesIncludeStableCode(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "GET", srv.URL+"/v1/sandboxes/does-not-exist", key, "")
	if code != 404 {
		t.Fatalf("want 404, got %d (%v)", code, out)
	}
	if out["code"] != "not_found" {
		t.Errorf("404 code = %v, want not_found", out["code"])
	}
	if _, ok := out["error"].(string); !ok {
		t.Errorf("error field must stay a string (backward compat), got %T", out["error"])
	}

	code, out = do(t, "GET", srv.URL+"/v1/sandboxes", "", "")
	if code != 401 {
		t.Fatalf("want 401, got %d (%v)", code, out)
	}
	if out["code"] != "unauthorized" {
		t.Errorf("401 code = %v, want unauthorized", out["code"])
	}
}

package httpapi

import "testing"

func TestAuditLogRecordsSandboxCreate(t *testing.T) {
	srv, key := newTestServer(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("create failed: %v", out)
	}

	code, list := do(t, "GET", srv.URL+"/v1/audit", key, "")
	if code != 200 {
		t.Fatalf("GET /v1/audit = %d %v", code, list)
	}
	entries, _ := list["entries"].([]any)
	found := false
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if m["action"] == "sandbox.create" && m["target"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("no sandbox.create audit entry for %s: %v", id, entries)
	}
}

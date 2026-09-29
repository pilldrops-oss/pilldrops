package httpapi

import "testing"

func TestSandboxPortsShapeAndOwnership(t *testing.T) {
	srv, key := newTestServer(t)

	// 9323 is not in the fixed list; the declared port must still be probed.
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","port":9323}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	code, resp := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/ports", key, "")
	if code != 200 {
		t.Fatalf("ports = %d (%v)", code, resp)
	}
	list, ok := resp["ports"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("ports = %v, want a non-empty list", resp["ports"])
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("entry %v is not an object", e)
		}
		if _, ok := m["port"]; !ok {
			t.Fatalf("entry missing port: %v", m)
		}
		if _, ok := m["listening"]; !ok {
			t.Fatalf("entry missing listening: %v", m)
		}
	}
	for _, e := range list {
		m := e.(map[string]any)
		if m["listening"] == true {
			t.Fatalf("port %v reported listening on a freshly-created sandbox", m["port"])
		}
	}

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/sb_missing/ports", key, ""); code != 404 {
		t.Fatalf("ports for missing sandbox = %d, want 404", code)
	}
}

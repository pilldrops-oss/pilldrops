package httpapi

import "testing"

func TestHostEndpoint(t *testing.T) {
	srv, key := newOperatorTestServer(t)
	code, out := do(t, "GET", srv.URL+"/v1/host", key, "")
	if code != 200 {
		t.Fatalf("GET /v1/host = %d", code)
	}
	for _, k := range []string{"mem_total_mb", "disk_total_gb", "sandboxes_active", "vcpu_committed", "mem_committed_mb"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("missing key %q in %v", k, out)
		}
	}
}

// The node-wide reads count every org's sandboxes and expose the machine's
// capacity, so a member key is refused on all three.
func TestNodeWideReadsNeedTheOperatorRole(t *testing.T) {
	srv, member := newTestServer(t)
	for _, path := range []string{"/v1/host", "/v1/nodes", "/metrics"} {
		if code, _ := getText(t, srv.URL+path, member); code != 403 {
			t.Errorf("GET %s with a member key = %d, want 403", path, code)
		}
	}
}

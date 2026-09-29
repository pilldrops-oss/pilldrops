package httpapi

import "testing"

func TestHostEndpoint(t *testing.T) {
	srv, key := newTestServer(t)
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

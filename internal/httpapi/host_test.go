package httpapi

import (
	"net/http"
	"testing"
)

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

// The control plane forwards a tenant's request under its operator key, so a
// node-only admin route must look at who the assertion names, not the wire key.
func TestAdminRoutesRefuseAForwardedTenant(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	url := srv.URL + "/v1/host"
	if code, _ := doAs(t, "GET", url, opKey, "u:cust1", "k-tenant", "user", "k-tenant", ""); code != http.StatusForbidden {
		t.Errorf("forwarded tenant = %d, want 403", code)
	}
	if code, _ := doAs(t, "GET", url, opKey, "u:cust1", "k-op", "operator", "k-op", ""); code != http.StatusOK {
		t.Errorf("forwarded operator = %d, want 200", code)
	}
	// The control plane's own hop names nobody; the operator wire key decides.
	if code, _ := doAs(t, "GET", url, opKey, "default", "", "", "", ""); code != http.StatusOK {
		t.Errorf("unnamed control-plane hop = %d, want 200", code)
	}
}

package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAPISpecServed(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("anonymous GET /openapi.json = %d, want 200 (public spec)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	b, _ := io.ReadAll(resp.Body)
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	if ver, _ := spec["openapi"].(string); !strings.HasPrefix(ver, "3") {
		t.Errorf("openapi version = %q, want 3.x", ver)
	}

	paths, _ := spec["paths"].(map[string]any)
	for _, p := range []string{"/health", "/v1/sandboxes", "/v1/sandboxes/{id}", "/v1/sandboxes/{id}/exec", "/v1/audit"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("spec missing path %s", p)
		}
	}

	comps, _ := spec["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	errSchema, _ := schemas["Error"].(map[string]any)
	props, _ := errSchema["properties"].(map[string]any)
	if _, ok := props["code"]; !ok {
		t.Errorf("Error schema missing 'code' property")
	}
}

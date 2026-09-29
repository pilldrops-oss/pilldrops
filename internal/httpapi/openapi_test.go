package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
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

// The docs are generated from openapi.json, so a route missing there is silently
// missing from the reference.
func TestEveryAgentRouteIsInTheSpec(t *testing.T) {
	// Scan the whole package; route registration is split across files.
	var src []byte
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		src = append(src, b...)
	}
	re := regexp.MustCompile(`route\("([A-Z]+) (/v1/sandboxes/\{id\}/agent[^"]*)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 20 {
		t.Fatalf("found only %d agent routes — the pattern has drifted from how routes are "+
			"registered, so this test is no longer checking anything", len(matches))
	}

	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	raw, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}

	var missing []string
	for _, m := range matches {
		method, path := strings.ToLower(m[1]), m[2]
		ops, ok := spec.Paths[path]
		if !ok {
			missing = append(missing, m[1]+" "+path+" (path absent)")
			continue
		}
		if _, ok := ops[method]; !ok {
			missing = append(missing, m[1]+" "+path+" (path present, method missing)")
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d agent route(s) are registered but not in openapi.json, so the generated "+
			"reference silently omits them:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

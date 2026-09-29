package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// A node whose sandboxes share the host kernel refuses the docker template at
// create, before anything boots, and says which runtime it needs. The test
// server runs the process runtime, so NestedDocker is false there for real
// rather than by a fake.
func TestDockerTemplateRefusedWithoutNestedDocker(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"docker"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create docker = %d, want 400 (it must be refused, not booted into a runtime that cannot host it): %v", code, out)
	}
	msg, _ := out["error"].(string)
	if msg == "" {
		msg, _ = out["message"].(string)
	}
	if !strings.Contains(msg, "firecracker") {
		t.Errorf("refusal %q does not name firecracker, so the caller cannot tell what would work", msg)
	}

	// Positive control in the same run: the refusal is about this template, not
	// about creates failing on this server.
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create base = %d, want 201 — the refusal above proves nothing if every create fails: %v", code, out)
	}
}

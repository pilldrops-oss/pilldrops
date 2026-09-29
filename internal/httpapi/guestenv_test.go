package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The guest learns its own id and preview root from the platform.
func TestCreateInjectsSandboxIDAndPreviewBase(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes",
		strings.NewReader(`{"template":"base","env":["TAG=prod"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		ID  string `json:"id"`
		URL string `json:"terminal_url"` // always present; the same appURL builder as app_url
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode >= 400 {
		t.Fatalf("create = %d err=%v", resp.StatusCode, err)
	}

	spec, ok := fake.LastBootSpec()
	if !ok {
		t.Fatal("nothing was booted")
	}
	// Later wins in both the Docker -e list and the Firecracker exec prefix.
	last := map[string]string{}
	for _, kv := range spec.Template.Env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	if last["PD_SANDBOX_ID"] != body.ID {
		t.Fatalf("PD_SANDBOX_ID = %q, want the created id %q (env: %v)", last["PD_SANDBOX_ID"], body.ID, spec.Template.Env)
	}
	if last["TAG"] != "prod" {
		t.Fatalf("caller env lost: %v", spec.Template.Env)
	}
	base := last["PD_PREVIEW_BASE"]
	if base == "" || !strings.HasPrefix(body.URL, base+"/") {
		t.Fatalf("PD_PREVIEW_BASE = %q does not prefix terminal_url %q", base, body.URL)
	}
}

// A caller cannot supply its own PD_* value. The platform appends its entries
// last, so shadowing would fail anyway, but the create is refused outright so
// the caller is told rather than silently overridden. Reserving the prefix is
// the guarantee; append order is only the belt to that braces.
func TestCreateRefusesReservedGuestEnvPrefix(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes",
		strings.NewReader(`{"template":"base","env":["PD_SANDBOX_ID=spoofed","TAG=prod"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with a reserved name = %d, want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if !strings.Contains(e.Error, "PD_") {
		t.Errorf("the refusal must name the reserved prefix, got %q", e.Error)
	}
	// Refused before anything booted: nothing spoofed reaches a guest.
	if _, booted := fake.LastBootSpec(); booted {
		t.Error("a create naming a reserved variable must not boot a sandbox")
	}
}

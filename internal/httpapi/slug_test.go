package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A slug is claimed at create or PATCH, refused with 409 while another live
// sandbox holds it, and freed by destroy.
func TestSlugIsClaimedRefusedAndFreed(t *testing.T) {
	srv, _, key := serviceTemplateServer(t)

	code, first := createJSON(t, srv, key, `{"template":"base","slug":"demo"}`)
	if code != http.StatusCreated || first["slug"] != "demo" {
		t.Fatalf("create with slug = %d %v, want 201 with slug demo", code, first)
	}
	firstID, _ := first["id"].(string)

	code, out := createJSON(t, srv, key, `{"template":"base","slug":"demo"}`)
	if code != http.StatusConflict || out["code"] != codeSlugTaken {
		t.Fatalf("second create = %d %v, want 409 %s", code, out, codeSlugTaken)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, `"demo"`) {
		t.Errorf("409 body %q does not name the slug", msg)
	}
	// Nothing was left behind by the refused create: the list holds one sandbox.
	if n := countSandboxes(t, srv, key); n != 1 {
		t.Fatalf("after a refused create the org has %d sandboxes, want 1", n)
	}

	// GET and list carry the slug.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sandboxes/"+firstID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["slug"] != "demo" {
		t.Errorf("GET slug = %v, want demo", got["slug"])
	}

	// PATCH onto a taken slug is 409; a bad grammar is 400; a fresh one is 200.
	code, second := createJSON(t, srv, key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("plain create = %d", code)
	}
	secondID, _ := second["id"].(string)
	if code, out := patchSandbox(t, srv, key, secondID, `{"slug":"demo"}`); code != http.StatusConflict || out["code"] != codeSlugTaken {
		t.Errorf("PATCH taken slug = %d %v, want 409", code, out)
	}
	if code, _ := patchSandbox(t, srv, key, secondID, `{"slug":"8000-x"}`); code != http.StatusBadRequest {
		t.Errorf("PATCH slug 8000-x = %d, want 400", code)
	}
	if code, out := patchSandbox(t, srv, key, secondID, `{"slug":"other"}`); code != http.StatusOK || out["slug"] != "other" {
		t.Errorf("PATCH fresh slug = %d %v, want 200 slug other", code, out)
	}

	// Destroy the holder; the same create now succeeds.
	del, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/sandboxes/"+firstID, nil)
	del.Header.Set("Authorization", "Bearer "+key)
	if resp, err := srv.Client().Do(del); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("destroy: %v %v", err, resp)
	}
	if code, out := createJSON(t, srv, key, `{"template":"base","slug":"demo"}`); code != http.StatusCreated {
		t.Fatalf("create after destroy = %d %v, want 201 (slug freed)", code, out)
	}
}

func countSandboxes(t *testing.T, srv *httptest.Server, key string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Sandboxes []json.RawMessage `json:"sandboxes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return len(out.Sandboxes)
}

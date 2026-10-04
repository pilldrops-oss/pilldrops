package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A single-port runtime (docker) publishes 8000 and the template's declared
// ports only; asking for anything else must be refused up front, naming the
// constraint, instead of 502ing after the sandbox is up.

func patchSandbox(t *testing.T, srv *httptest.Server, key, id, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/sandboxes/"+id, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func createJSON(t *testing.T, srv *httptest.Server, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestSinglePortRuntimeRefusesAnUndeclaredPort(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)
	if fake.MultiPort {
		t.Fatal("fixture advertises MultiPort; this test needs the single-port runtime")
	}

	code, out := createJSON(t, srv, key, `{"template":"base","port":3000}`)
	if code != http.StatusBadRequest || out["code"] != codePortNotPublished {
		t.Fatalf("port 3000 on a single-port runtime = %d %v, want 400 %s", code, out, codePortNotPublished)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "8000") || !strings.Contains(msg, "declared") {
		t.Errorf("error %q does not name the constraint (8000 + declared ports)", msg)
	}
	if code, _ := createJSON(t, srv, key, `{"template":"base","port":8000}`); code != http.StatusCreated {
		t.Fatalf("port 8000 = %d, want 201 (positive control)", code)
	}
	// A port the request itself declares is fine.
	if code, out := createJSON(t, srv, key, `{"template":"base","ports":[3000],"port":3000}`); code != http.StatusCreated {
		t.Fatalf("request-declared port 3000 = %d %v, want 201", code, out)
	}
}

func TestMultiPortRuntimeAcceptsAnyPort(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)
	fake.MultiPort = true
	if code, out := createJSON(t, srv, key, `{"template":"base","port":3000}`); code != http.StatusCreated {
		t.Fatalf("port 3000 on a multi-port runtime = %d %v, want 201", code, out)
	}
}

func TestPatchPortMovesThePreviewWithoutClearingTheName(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)
	fake.MultiPort = true
	code, created := createJSON(t, srv, key, `{"template":"base","name":"web"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	id, _ := created["id"].(string)

	code, out := patchSandbox(t, srv, key, id, `{"port":3000}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH {port} = %d %v", code, out)
	}
	if out["name"] != "web" {
		t.Errorf("PATCH {port} cleared the name: %v", out["name"])
	}
	if u, _ := out["url"].(string); !strings.Contains(u, "/3000/") && !strings.HasPrefix(u, "https://3000-") {
		t.Errorf("url = %q, want the preview on port 3000", u)
	}
	// GET agrees, and a later name-only PATCH keeps the port.
	if _, out = patchSandbox(t, srv, key, id, `{"name":"web2"}`); out["name"] != "web2" {
		t.Fatalf("rename: %v", out)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sandboxes/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if u, _ := got["url"].(string); !strings.Contains(u, "/3000/") && !strings.HasPrefix(u, "https://3000-") {
		t.Errorf("after rename, GET url = %q, want port 3000 kept", u)
	}

	// Out-of-range and, on a single-port runtime, undeclared ports are refused.
	if code, _ := patchSandbox(t, srv, key, id, `{"port":70000}`); code != http.StatusBadRequest {
		t.Errorf("port 70000 = %d, want 400", code)
	}
	fake.MultiPort = false
	if code, out := patchSandbox(t, srv, key, id, `{"port":3001}`); code != http.StatusBadRequest || out["code"] != codePortNotPublished {
		t.Errorf("single-port PATCH {port:3001} = %d %v, want 400 %s", code, out, codePortNotPublished)
	}
}

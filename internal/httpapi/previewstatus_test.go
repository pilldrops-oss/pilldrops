package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestClassifyPreview(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    bool
		want   string
	}{
		{"200 is ready", 200, false, "ready"},
		{"204 is ready", 204, false, "ready"},
		{"301 is ready", 301, false, "ready"},
		{"302 is ready", 302, false, "ready"},
		{"401 is ready (app answered)", 401, false, "ready"},
		{"404 means serving but no page at /", 404, false, "no_page"},
		{"502 means still starting", 502, false, "starting"},
		{"503 means still starting", 503, false, "starting"},
		{"504 means still starting", 504, false, "starting"},
		{"500 is an app error, not a boot state", 500, false, "app_error"},
		{"transport failure means still starting", 0, true, "starting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPreview(c.status, c.err); got != c.want {
				t.Fatalf("classifyPreview(%d, %v) = %q, want %q", c.status, c.err, got, c.want)
			}
		})
	}
}

// previewStatusOf calls the endpoint and decodes the body.
func previewStatusOf(t *testing.T, srv, key, id string) map[string]any {
	t.Helper()
	code, body := do(t, "GET", srv+"/v1/sandboxes/"+id+"/preview-status", key, "")
	if code != http.StatusOK {
		t.Fatalf("preview-status = %d (%v), want 200", code, body)
	}
	return body
}

// With no deployed app the preview serves the terminal, which must not read as "ready".
func TestPreviewStatusReportsTerminalWhenNoApp(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t) // template "base": no start cmd, no port
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "terminal" {
		t.Fatalf("state = %v, want terminal", body["state"])
	}
}

// withDeployedApp publishes a port so the handler probes, and stubs the probe's answer.
func withDeployedApp(t *testing.T, api *Server, id string, status int, perr error) {
	t.Helper()
	if err := api.d.SetPublicPort(t.Context(), id, 8000); err != nil {
		t.Fatalf("SetPublicPort: %v", err)
	}
	api.probePreview = func(context.Context, string) (int, error) { return status, perr }
}

func TestPreviewStatusReportsReady(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 200, nil)
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "ready" {
		t.Fatalf("state = %v, want ready", body["state"])
	}
	if body["upstream_status"] != float64(200) {
		t.Fatalf("upstream_status = %v, want 200", body["upstream_status"])
	}
}

func TestPreviewStatusReportsStartingWhileBooting(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 503, nil)
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "starting" {
		t.Fatalf("state = %v, want starting", body["state"])
	}
}

func TestPreviewStatusReportsAppError(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 500, nil)
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "app_error" {
		t.Fatalf("state = %v, want app_error", body["state"])
	}
}

func TestPreviewStatusTransportErrorIsStarting(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 0, errors.New("connection refused"))
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "starting" {
		t.Fatalf("state = %v, want starting", body["state"])
	}
	if _, present := body["upstream_status"]; present {
		t.Fatal("upstream_status must be absent when the probe never completed")
	}
}

// A status poll must never wake, and bill, a paused sandbox.
func TestPreviewStatusDoesNotWakePaused(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 200, nil)
	if code, out := do(t, "POST", srv+"/v1/sandboxes/"+sb+"/pause", key, ""); code != http.StatusOK {
		t.Fatalf("pause = %d (%s)", code, out)
	}
	api.probePreview = func(context.Context, string) (int, error) {
		t.Fatal("probe ran against a paused sandbox — a status poll must not wake it")
		return 0, nil
	}
	body := previewStatusOf(t, srv, key, sb)
	if body["state"] != "paused" {
		t.Fatalf("state = %v, want paused", body["state"])
	}
	if got, _ := do(t, "GET", srv+"/v1/sandboxes/"+sb, key, ""); got != http.StatusOK {
		t.Fatalf("get after status poll = %d", got)
	}
}

func TestPreviewStatusCrossTenant404s(t *testing.T) {
	srv, _, sb, st := newCronSandbox(t)
	_, otherKey, err := st.CreateKey(t.Context(), "other", "org_other")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "GET", srv+"/v1/sandboxes/"+sb+"/preview-status", otherKey, ""); code != http.StatusNotFound {
		t.Fatalf("cross-tenant preview-status = %d, want 404", code)
	}
	if code, _ := do(t, "GET", srv+"/v1/sandboxes/sb_nope/preview-status", otherKey, ""); code != http.StatusNotFound {
		t.Fatalf("unknown sandbox = %d, want 404", code)
	}
}

func TestPreviewStatusDefaultsToPublishedPort(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)
	withDeployedApp(t, api, sb, 200, nil)
	body := previewStatusOf(t, srv, key, sb)
	if body["port"] != float64(8000) {
		t.Fatalf("port = %v, want the published 8000", body["port"])
	}
}
